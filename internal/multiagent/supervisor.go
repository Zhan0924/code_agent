package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/planner"
	"go.uber.org/zap"
)

// Supervisor orchestrates multiple sub-agents to complete a complex task.
// It receives a plan (DAG), assigns steps to specialized sub-agents based on
// step type, and coordinates their execution with bounded parallelism.
type Supervisor struct {
	pool             *AgentPool
	bus              *MessageBus
	dispatcher       ToolDispatcher
	conflictResolver *ConflictResolver
	roleSelector     *RoleSelector
	config           SupervisorConfig
	logger           *zap.Logger
}

// NewSupervisor creates a multi-agent supervisor.
func NewSupervisor(dispatcher ToolDispatcher, config SupervisorConfig, logger *zap.Logger) *Supervisor {
	return &Supervisor{
		pool:             NewAgentPool(config.MaxParallel, logger),
		bus:              NewMessageBus(logger),
		dispatcher:       dispatcher,
		conflictResolver: NewConflictResolver(StrategyPriority, logger),
		roleSelector:     NewRoleSelector(logger),
		config:           config,
		logger:           logger.With(zap.String("component", "multiagent.supervisor")),
	}
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

func (s *Supervisor) executeStep(ctx context.Context, step planner.Step) AgentResult {
	// Use dynamic role selection instead of static classification
	candidates := CandidatesForAction(step.Action)
	agentType := s.roleSelector.SelectBest(step.Action, candidates)
	start := time.Now()

	stepCtx, cancel := context.WithTimeout(ctx, s.config.StepTimeout)
	defer cancel()

	agent := s.pool.Acquire(agentType)
	defer s.pool.Release(agent)

	// Pre-check file conflicts before executing write actions
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
		output, err := agent.Execute(stepCtx, DelegationRequest{
			StepID:     step.ID,
			AgentType:  agentType,
			Action:     step.Action,
			Task:       step.Description,
			Parameters: step.Parameters,
		}, s.dispatcher)

		result.Duration = time.Since(start)
		if err != nil {
			result.Success = false
			result.Error = err.Error()
		} else {
			result.Success = true
			result.Output = output
		}
	}

	// Record result for dynamic role selection learning
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
