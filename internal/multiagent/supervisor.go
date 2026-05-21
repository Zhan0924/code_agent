package multiagent

import (
	"context"
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
	pool       *AgentPool
	bus        *MessageBus
	dispatcher ToolDispatcher
	config     SupervisorConfig
	logger     *zap.Logger
}

// NewSupervisor creates a multi-agent supervisor.
func NewSupervisor(dispatcher ToolDispatcher, config SupervisorConfig, logger *zap.Logger) *Supervisor {
	return &Supervisor{
		pool:       NewAgentPool(config.MaxParallel, logger),
		bus:        NewMessageBus(logger),
		dispatcher: dispatcher,
		config:     config,
		logger:     logger.With(zap.String("component", "multiagent.supervisor")),
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
	agentType := classifyStep(step)
	start := time.Now()

	stepCtx, cancel := context.WithTimeout(ctx, s.config.StepTimeout)
	defer cancel()

	agent := s.pool.Acquire(agentType)
	defer s.pool.Release(agent)

	output, err := agent.Execute(stepCtx, DelegationRequest{
		StepID:    step.ID,
		AgentType: agentType,
		Task:      step.Description,
		Parameters: step.Parameters,
	}, s.dispatcher)

	result := AgentResult{
		AgentID:   agent.ID,
		AgentType: agentType,
		StepID:    step.ID,
		Duration:  time.Since(start),
	}

	if err != nil {
		result.Success = false
		result.Error = err.Error()
	} else {
		result.Success = true
		result.Output = output
	}

	s.bus.Publish(Message{
		From:      agent.ID,
		To:        "supervisor",
		Type:      "step_complete",
		Content:   fmt.Sprintf("step=%s success=%v", step.ID, result.Success),
		Timestamp: time.Now(),
	})

	return result
}

func classifyStep(step planner.Step) AgentType {
	switch step.Action {
	case "write_file", "edit_file", "patch_file", "create_directory",
		"git_commit", "git_branch":
		return AgentCode
	case "run_tests", "execute_code":
		return AgentTest
	case "read_file", "search_code", "list_files":
		return AgentReview
	default:
		return AgentCode
	}
}
