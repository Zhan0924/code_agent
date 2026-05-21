package planner

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

// HierarchicalPlanner decomposes complex goals into sub-goals, each with its own plan.
// Used when EstimateComplexity > 12.
type HierarchicalPlanner struct {
	decomposer *GoalDecomposer
	planner    *Planner
	tracker    *ProgressTracker
	executor   *Executor
	logger     *zap.Logger
}

// NewHierarchicalPlanner creates a hierarchical planner.
func NewHierarchicalPlanner(llm LLMCaller, stepExec StepExecutor, logger *zap.Logger) *HierarchicalPlanner {
	p := NewPlanner(llm, logger)
	return &HierarchicalPlanner{
		decomposer: NewGoalDecomposer(llm, logger),
		planner:    p,
		tracker:    NewProgressTracker(logger),
		executor:   NewExecutor(p, stepExec, logger),
		logger:     logger.With(zap.String("component", "planner.hierarchical")),
	}
}

// HierarchicalResult holds the outcome of a hierarchical execution.
type HierarchicalResult struct {
	Goal       string           `json:"goal"`
	SubGoals   []SubGoal        `json:"sub_goals"`
	Results    []*ExecutionResult `json:"results"`
	Success    bool             `json:"success"`
	Progress   float64          `json:"progress"`
}

// Execute runs a hierarchical plan: decompose → plan each sub-goal → execute in order.
func (hp *HierarchicalPlanner) Execute(ctx context.Context, goal string, contextInfo string) (*HierarchicalResult, error) {
	subGoals, err := hp.decomposer.Decompose(ctx, goal, contextInfo)
	if err != nil {
		return nil, fmt.Errorf("goal decomposition failed: %w", err)
	}

	hp.tracker.Track(subGoals)

	result := &HierarchicalResult{
		Goal:     goal,
		SubGoals: subGoals,
		Results:  make([]*ExecutionResult, 0, len(subGoals)),
	}

	for i := range subGoals {
		sg := &subGoals[i]
		hp.tracker.MarkStarted(sg.ID)

		hp.logger.Info("executing sub-goal",
			zap.String("id", sg.ID),
			zap.String("description", sg.Description),
			zap.Int("priority", sg.Priority))

		plan, err := hp.planner.CreatePlan(ctx, sg.Description, contextInfo)
		if err != nil {
			hp.logger.Warn("plan creation failed for sub-goal", zap.String("id", sg.ID), zap.Error(err))
			if needsReplan := hp.tracker.MarkFailed(sg.ID); needsReplan {
				hp.logger.Error("sub-goal exhausted retries", zap.String("id", sg.ID))
			}
			continue
		}

		sg.Plan = plan
		execResult, err := hp.executor.Execute(ctx, plan)
		result.Results = append(result.Results, execResult)

		if err != nil || !execResult.Success {
			if needsReplan := hp.tracker.MarkFailed(sg.ID); needsReplan {
				hp.logger.Warn("sub-goal failed, attempting replan", zap.String("id", sg.ID))
				if replanned := hp.replanSubGoal(ctx, sg, contextInfo); replanned {
					hp.tracker.MarkCompleted(sg.ID)
				}
			}
		} else {
			hp.tracker.MarkCompleted(sg.ID)
		}
	}

	result.Progress = hp.tracker.Progress()
	result.Success = result.Progress >= 1.0

	hp.logger.Info("hierarchical execution complete",
		zap.String("goal", goal),
		zap.Float64("progress", result.Progress),
		zap.Bool("success", result.Success))

	return result, nil
}

func (hp *HierarchicalPlanner) replanSubGoal(ctx context.Context, sg *SubGoal, contextInfo string) bool {
	newDesc := fmt.Sprintf("(Retry) %s — previous attempt failed, try a different approach", sg.Description)
	plan, err := hp.planner.CreatePlan(ctx, newDesc, contextInfo)
	if err != nil {
		return false
	}
	sg.Plan = plan
	execResult, err := hp.executor.Execute(ctx, plan)
	return err == nil && execResult.Success
}

// NeedsHierarchical returns true if the task complexity warrants hierarchical planning.
func NeedsHierarchical(userMessage string) bool {
	return EstimateComplexity(userMessage) >= 12
}
