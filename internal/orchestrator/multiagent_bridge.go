// multiagent_bridge.go — optional multi-agent Supervisor integration.
//
// When attached, the orchestrator can delegate complex plans to the
// multiagent.Supervisor which runs steps in parallel using specialized
// sub-agents (code, test, review). This is an alternative execution
// backend to the serial planner.Executor — it's chosen when a plan has
// enough independent steps to benefit from parallelism.
//
// Wiring (in main.go or a setup function):
//
//	orch.AttachSupervisor(multiagent.NewSupervisor(adapter, cfg, logger))
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/metrics"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/multiagent"
	"github.com/agent/code_agent/internal/planner"
	"go.uber.org/zap"
)

// toolDispatcherAdapter bridges orchestrator.executeTool to multiagent.ToolDispatcher.
type toolDispatcherAdapter struct {
	orch *Orchestrator
}

func (a *toolDispatcherAdapter) Dispatch(ctx context.Context, toolName string, args json.RawMessage) (string, error) {
	if args == nil {
		args = json.RawMessage(`{}`)
	}
	tc := models.ToolCall{
		ID:   fmt.Sprintf("ma-%s", toolName),
		Name: toolName,
		Args: args,
	}
	result, err := a.orch.executeTool(ctx, tc)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", nil
	}
	if result.IsError {
		return result.Content, fmt.Errorf("tool error: %s", truncateLine(result.Content, 200))
	}
	return result.Content, nil
}

// orchToolExecutor adapts the orchestrator's executeTool to agentloop.ToolExecutor.
type orchToolExecutor struct {
	orch *Orchestrator
}

func (e *orchToolExecutor) Execute(ctx context.Context, tc models.ToolCall) (*models.ToolResult, error) {
	return e.orch.executeTool(ctx, tc)
}

// orchToolProvider adapts the orchestrator's tool definitions to agentloop.ToolProvider.
type orchToolProvider struct {
	orch *Orchestrator
}

func (p *orchToolProvider) Definitions() []models.ToolDefinition {
	return p.orch.GetAvailableTools()
}

// AttachSupervisor wires the multi-agent Supervisor into the orchestrator.
// Call after NewOrchestrator and before the first ProcessMessage.
func (o *Orchestrator) AttachSupervisor(sup *multiagent.Supervisor) {
	if sup == nil {
		return
	}
	o.supervisor = sup
	o.logger.Info("multi-agent supervisor attached")
}

// NewToolDispatcherAdapter creates a multiagent.ToolDispatcher backed by
// the orchestrator's tool execution pipeline.
func (o *Orchestrator) NewToolDispatcherAdapter() multiagent.ToolDispatcher {
	return &toolDispatcherAdapter{orch: o}
}

// NewSupervisorWithReAct creates a Supervisor with full ReAct capabilities
// injected from the orchestrator's LLM client and tool registry.
func (o *Orchestrator) NewSupervisorWithReAct(config multiagent.SupervisorConfig) *multiagent.Supervisor {
	return multiagent.NewSupervisor(
		o.NewToolDispatcherAdapter(),
		config,
		o.logger,
		multiagent.WithLLM(o.llmClient),
		multiagent.WithToolExecutor(&orchToolExecutor{orch: o}),
		multiagent.WithToolProvider(&orchToolProvider{orch: o}),
		multiagent.WithEventSink(agentloop.NoopSink{}),
	)
}

// planHasParallelism checks if a plan's DAG has at least one level with
// multiple independent steps — i.e., there's actual benefit to multi-agent
// parallel execution vs serial.
func planHasParallelism(plan *planner.Plan) bool {
	levels, err := planner.TopologicalSort(plan.Steps)
	if err != nil {
		return false
	}
	for _, level := range levels {
		if len(level) >= 2 {
			return true
		}
	}
	return false
}

// executePlanWithSupervisor delegates plan execution to the multi-agent
// Supervisor, which runs independent steps in parallel using specialized
// sub-agents.
func (o *Orchestrator) executePlanWithSupervisor(ctx context.Context, task *models.Task, plan *planner.Plan) (*models.ChatResponse, bool, error) {
	o.logger.Info("executing plan via multi-agent Supervisor",
		zap.String("task_id", task.ID),
		zap.Int("steps", len(plan.Steps)))

	result, err := o.supervisor.Execute(ctx, plan)
	if err != nil {
		return nil, false, fmt.Errorf("supervisor execution: %w", err)
	}

	// Update plan step statuses from supervisor results
	for _, r := range result.Results {
		for i := range plan.Steps {
			if plan.Steps[i].ID == r.StepID {
				if r.Success {
					plan.Steps[i].Status = planner.StepCompleted
					plan.Steps[i].Output = r.Output
				} else {
					plan.Steps[i].Status = planner.StepFailed
					plan.Steps[i].Error = r.Error
				}
			}
		}
	}

	// Persist final plan state
	o.persistPlan(ctx, task.ID, plan)

	// Record metrics
	for _, step := range plan.Steps {
		status := string(step.Status)
		metrics.PlannerStepsTotal.WithLabelValues(step.Action, status).Inc()
	}

	return &models.ChatResponse{
		SessionID: task.SessionID,
		TaskID:    task.ID,
		State:     plannedChatState(result.Success),
		Message:   formatSupervisorResult(result, plan),
	}, true, nil
}

func formatSupervisorResult(r *multiagent.SupervisorResult, plan *planner.Plan) string {
	var b strings.Builder
	if r.Success {
		b.WriteString("## Plan completed (multi-agent)\n\n")
	} else {
		b.WriteString("## Plan partially failed (multi-agent)\n\n")
	}
	fmt.Fprintf(&b, "%s (duration: %s)\n\n### Steps\n", r.Summary, r.Duration.Round(time.Millisecond))
	for _, step := range plan.Steps {
		icon := stepStatusIcon(step.Status)
		fmt.Fprintf(&b, "- %s **%s** (`%s`): %s\n", icon, step.ID, step.Action, step.Description)
		if step.Error != "" {
			fmt.Fprintf(&b, "    - error: %s\n", truncateLine(step.Error, 200))
		}
	}
	return b.String()
}
