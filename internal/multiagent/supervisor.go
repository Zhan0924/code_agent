package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/planner"
	"go.uber.org/zap"
)

// Supervisor orchestrates multiple sub-agents to complete a complex task.
type Supervisor struct {
	pool             *AgentPool
	bus              *MessageBus
	dispatcher       ToolDispatcher
	conflictResolver *ConflictResolver
	roleSelector     *RoleSelector
	config           SupervisorConfig
	logger           *zap.Logger

	// Optional dependencies for ReAct-capable sub-agents
	llmCaller    agentloop.LLMCaller
	toolExecutor agentloop.ToolExecutor
	toolProvider agentloop.ToolProvider
	eventSink    agentloop.EventSink
}

// SupervisorOption configures optional Supervisor features.
type SupervisorOption func(*Supervisor)

// WithLLM sets the LLM caller for ReAct sub-agents.
// ReAct also requires WithToolExecutor and WithToolProvider to be set.
func WithLLM(llm agentloop.LLMCaller) SupervisorOption {
	return func(s *Supervisor) { s.llmCaller = llm }
}

// WithToolExecutor sets the tool executor for ReAct sub-agents.
func WithToolExecutor(te agentloop.ToolExecutor) SupervisorOption {
	return func(s *Supervisor) { s.toolExecutor = te }
}

// WithToolProvider sets the tool provider for ReAct sub-agents.
func WithToolProvider(tp agentloop.ToolProvider) SupervisorOption {
	return func(s *Supervisor) { s.toolProvider = tp }
}

// WithEventSink sets the event sink for ReAct sub-agents.
func WithEventSink(sink agentloop.EventSink) SupervisorOption {
	return func(s *Supervisor) { s.eventSink = sink }
}

// NewSupervisor creates a multi-agent supervisor.
func NewSupervisor(dispatcher ToolDispatcher, config SupervisorConfig, logger *zap.Logger, opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{
		pool:             NewAgentPool(config.MaxParallel, logger),
		bus:              NewMessageBus(logger),
		dispatcher:       dispatcher,
		conflictResolver: NewConflictResolver(StrategyPriority, logger),
		roleSelector:     NewRoleSelector(logger),
		config:           config,
		logger:           logger.With(zap.String("component", "multiagent.supervisor")),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SupervisorResult holds the outcome of a supervised multi-agent execution.
type SupervisorResult struct {
	Success  bool          `json:"success"`
	Results  []AgentResult `json:"results"`
	Duration time.Duration `json:"duration"`
	Summary  string        `json:"summary"`
}

// Execute runs a plan using multiple sub-agents.
func (s *Supervisor) Execute(ctx context.Context, plan *planner.Plan) (*SupervisorResult, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, s.config.TotalTimeout)
	defer cancel()

	levels, err := planner.TopologicalSort(plan.Steps)
	if err != nil {
		return nil, fmt.Errorf("invalid plan DAG: %w", err)
	}

	var allResults []AgentResult

	for levelIdx, level := range levels {
		s.logger.Info("executing plan level",
			zap.Int("level", levelIdx),
			zap.Int("steps", len(level)))

		levelResults, err := s.executeLevel(ctx, level)
		if err != nil {
			return &SupervisorResult{
				Success:  false,
				Results:  allResults,
				Duration: time.Since(start),
				Summary:  fmt.Sprintf("Failed at level %d: %v", levelIdx, err),
			}, nil
		}
		allResults = append(allResults, levelResults...)

		for _, r := range levelResults {
			if !r.Success {
				return &SupervisorResult{
					Success:  false,
					Results:  allResults,
					Duration: time.Since(start),
					Summary:  fmt.Sprintf("Step %s failed: %s", r.StepID, r.Error),
				}, nil
			}
		}
	}

	return &SupervisorResult{
		Success:  true,
		Results:  allResults,
		Duration: time.Since(start),
		Summary:  fmt.Sprintf("All %d steps completed successfully", len(allResults)),
	}, nil
}

func (s *Supervisor) executeLevel(ctx context.Context, steps []planner.Step) ([]AgentResult, error) {
	var wg sync.WaitGroup
	results := make([]AgentResult, len(steps))

	for i, step := range steps {
		wg.Add(1)
		go func(idx int, st planner.Step) {
			defer wg.Done()
			results[idx] = s.executeStep(ctx, st)
		}(i, step)
	}

	wg.Wait()
	return results, nil
}

// buildAgentDeps constructs AgentDeps if LLM and tool deps are all available.
func (s *Supervisor) buildAgentDeps() *AgentDeps {
	if s.llmCaller == nil || s.toolExecutor == nil || s.toolProvider == nil {
		return nil
	}
	return &AgentDeps{
		LLM:          s.llmCaller,
		ToolExecutor: s.toolExecutor,
		ToolProvider: s.toolProvider,
		EventSink:    s.eventSink,
	}
}

func (s *Supervisor) executeStep(ctx context.Context, step planner.Step) AgentResult {
	candidates := CandidatesForAction(step.Action)
	agentType := s.roleSelector.SelectBest(step.Action, candidates)
	start := time.Now()

	stepCtx, cancel := context.WithTimeout(ctx, s.config.StepTimeout)
	defer cancel()

	agent := s.pool.Acquire(agentType)
	defer s.pool.Release(agent)

	var conflictBlocked bool
	var conflictMsg string
	if isFileWriteAction(step.Action) {
		filePath := extractFilePathFromParams(step.Parameters)
		if filePath != "" {
			edit := FileEdit{
				AgentID:   agent.ID,
				FilePath:  filePath,
				Action:    step.Action,
				Timestamp: time.Now(),
			}
			if conflict := s.conflictResolver.RecordEdit(edit); conflict != nil {
				winner := s.conflictResolver.Resolve(conflict)
				if winner.AgentID != agent.ID {
					conflictBlocked = true
					conflictMsg = fmt.Sprintf("conflict on %s: resolved in favor of %s", filePath, winner.AgentID)
				}
			}
		}
	}

	result := AgentResult{
		AgentID:   agent.ID,
		AgentType: agentType,
		StepID:    step.ID,
	}

	if conflictBlocked {
		result.Success = false
		result.Error = conflictMsg
		result.Duration = time.Since(start)
	} else {
		req := DelegationRequest{
			StepID:            step.ID,
			AgentType:         agentType,
			Action:            step.Action,
			Task:              step.Description,
			Parameters:        step.Parameters,
			ReasoningRequired: step.ReasoningRequired,
		}

		deps := s.buildAgentDeps()
		output, err := agent.ExecuteWithDeps(stepCtx, req, s.dispatcher, deps)

		result.Duration = time.Since(start)
		if err != nil {
			result.Success = false
			result.Error = err.Error()
		} else {
			result.Success = true
			result.Output = output
		}
	}

	s.roleSelector.RecordResult(agentType, result.Success, result.Duration)

	s.bus.Publish(Message{
		From:      agent.ID,
		To:        "supervisor",
		Type:      "step_complete",
		Content:   fmt.Sprintf("step=%s success=%v", step.ID, result.Success),
		Timestamp: time.Now(),
	})

	return result
}

func isFileWriteAction(action string) bool {
	switch action {
	case "write_file", "edit_file", "patch_file":
		return true
	}
	return false
}

func extractFilePathFromParams(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	if p.FilePath != "" {
		return p.FilePath
	}
	return p.Path
}
