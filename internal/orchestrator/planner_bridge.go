// planner_bridge.go — optional Planner integration for complex requests.
//
// Historically the Orchestrator always drove a flat ReAct loop: the LLM would
// think, call a tool, observe, think again, up to N steps. That works well
// for small tasks but struggles on multi-file refactors ("update the auth
// module across all services, add migration, run tests") where LLMs tend to
// forget they were in the middle of a plan.
//
// This bridge provides a hook the Orchestrator can call to materialize an
// explicit DAG (plan) via internal/planner, execute it with bounded
// parallelism (see planner.Executor), and then hand control back to the
// ReAct loop for any remaining conversational wrap-up. It's intentionally
// opt-in and the existing behaviour is preserved when unused:
//
//	// Optional, call from NewOrchestrator / a setter:
//	orch.AttachPlanner(planner.NewPlanner(...), ...)
//
// The orchestrator then calls MaybeUsePlanner inside ProcessMessage. If no
// planner is attached, the call is a fast no-op.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/metrics"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/planner"
	"go.uber.org/zap"
)

// PlanStore abstracts plan persistence so the planner bridge doesn't depend
// directly on *store.Store (testability + nil-safety).
type PlanStore interface {
	SavePlan(ctx context.Context, taskID string, planJSON []byte) error
	LoadPlan(ctx context.Context, taskID string) ([]byte, error)
}

// ─── LLM Adapter for Planner ───────────────────────────────────────────────

// llmCallerAdapter adapts llm.Client to the planner.LLMCaller interface.
type llmCallerAdapter struct {
	client *llm.Client
}

// NewLLMCallerAdapter creates a planner.LLMCaller backed by the shared LLM client.
func NewLLMCallerAdapter(c *llm.Client) planner.LLMCaller {
	return &llmCallerAdapter{client: c}
}

func (a *llmCallerAdapter) Call(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	resp, err := a.client.ChatCompletion(ctx, &llm.ChatRequest{
		Messages: []models.Message{
			{Role: models.RoleSystem, Content: systemPrompt},
			{Role: models.RoleUser, Content: userPrompt},
		},
		Temperature: 0.2,
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// plannerComponents holds the optional Planner wiring. A nil plannerComponents
// on an Orchestrator disables the plan-first code path cleanly.
type plannerComponents struct {
	planner  *planner.Planner
	executor *planner.Executor
}

// AttachPlanner wires a Planner + Executor into the orchestrator. Call this
// after NewOrchestrator and before the first ProcessMessage. Thread-safety:
// this is not safe for concurrent callers; it's meant to be called once at
// startup.
//
// The StepExecutor passed to the planner is a thin adapter that routes plan
// steps through the orchestrator's existing tool-dispatch path, so tools
// remain a single source of truth.
func (o *Orchestrator) AttachPlanner(p *planner.Planner) {
	if p == nil {
		return
	}
	exec := planner.NewExecutor(p, o.executePlanStep, o.logger)
	// Default parallelism of 2 is safe: step execution often opens a sandbox
	// container or runs an LLM sub-call, both of which contend for resources.
	exec.SetMaxParallelism(2)
	o.planner = &plannerComponents{planner: p, executor: exec}
}

// NOTE: the `planner *plannerComponents` field is declared on the
// Orchestrator struct in orchestrator.go. We keep the methods here so that
// the main file stays focused on the ReAct loop.

// MaybeUsePlanner decides whether a user request should go through the
// Planner (DAG execution) instead of the default flat ReAct loop. The
// decision is based on a lightweight complexity heuristic and whether a
// planner is attached.
//
// Returns (result, true, nil) if the planner was used and succeeded.
// Returns (nil, false, nil) if the planner was skipped (not attached, task
// too simple, etc.) — in that case the caller should continue with the
// normal ReAct path. An error return means the planner was attempted but
// fundamentally failed and the caller should surface the error.
func (o *Orchestrator) MaybeUsePlanner(ctx context.Context, task *models.Task) (*models.ChatResponse, bool, error) {
	if o.planner == nil || o.planner.planner == nil {
		return nil, false, nil
	}
	// Only escalate to a DAG plan for sufficiently complex requests. The
	// heuristic is intentionally cheap (string scan, no LLM call) so we can
	// gate on it per request.
	if !planner.NeedsPlanning(task.UserInput) {
		return nil, false, nil
	}

	o.logger.Info("routing through Planner",
		zap.String("task_id", task.ID),
		zap.String("session_id", task.SessionID),
		zap.Int("complexity", planner.EstimateComplexity(task.UserInput)),
	)

	// contextInfo is a short hint gathered from the current session's long-term
	// memory — it helps the Planner generate steps grounded in the active code
	// context without having to requery RAG itself.
	var contextInfo string
	if sess, _ := o.sessionMgr.Get(ctx, task.SessionID); sess != nil {
		contextInfo = sess.Summary
	}
	plan, err := o.planner.planner.CreatePlan(ctx, task.UserInput, contextInfo)
	if err != nil {
		// A failure to create a plan is not fatal — we just fall back to the
		// ReAct loop. Log and return (nil, false, nil) so the caller doesn't
		// have to distinguish "no plan" from "failed to plan".
		o.logger.Warn("plan creation failed, falling back to ReAct",
			zap.String("task_id", task.ID), zap.Error(err))
		return nil, false, nil
	}
	metrics.PlannerPlansCreated.Inc()

	// Persist initial plan state
	o.persistPlan(ctx, task.ID, plan)

	// Choose execution backend: multi-agent Supervisor for plans with
	// parallelizable steps, serial Executor otherwise.
	if o.supervisor != nil && planHasParallelism(plan) {
		return o.executePlanWithSupervisor(ctx, task, plan)
	}

	execResult, err := o.planner.executor.Execute(ctx, plan)
	if err != nil {
		return nil, false, fmt.Errorf("plan execution: %w", err)
	}

	// Persist final plan state (with step outputs/statuses)
	o.persistPlan(ctx, task.ID, execResult.Plan)

	// Record per-step outcomes into metrics for operators to see which step
	// kinds fail most often.
	for _, step := range execResult.Plan.Steps {
		status := string(step.Status)
		metrics.PlannerStepsTotal.WithLabelValues(step.Action, status).Inc()
	}
	if !execResult.Success {
		// Executor already revised up to MaxRevision times. Any remaining
		// failure counts as one "final" revision signal for metrics.
		metrics.PlannerRevisionTotal.Inc()
	}

	return &models.ChatResponse{
		SessionID: task.SessionID,
		TaskID:    task.ID,
		State:     plannedChatState(execResult.Success),
		Message:   formatPlanResult(execResult),
	}, true, nil
}

func plannedChatState(success bool) models.TaskState {
	if success {
		return models.TaskStateCompleted
	}
	return models.TaskStateFailed
}

// formatPlanResult renders the Executor's ExecutionResult into a user-facing
// markdown message. The goal is to give the user a scannable summary instead
// of a raw tool log — the individual step outputs are still available in
// structured form via the task/plan persistence layer.
func formatPlanResult(r *planner.ExecutionResult) string {
	var b strings.Builder
	if r.Success {
		b.WriteString("## ✅ Plan completed\n\n")
	} else {
		b.WriteString("## ⚠️ Plan did not fully succeed\n\n")
	}
	b.WriteString(r.Summary)
	b.WriteString("\n\n### Steps\n")
	for _, step := range r.Plan.Steps {
		icon := stepStatusIcon(step.Status)
		fmt.Fprintf(&b, "- %s **%s** (`%s`): %s\n", icon, step.ID, step.Action, step.Description)
		if step.Error != "" {
			fmt.Fprintf(&b, "    - error: %s\n", truncateLine(step.Error, 200))
		}
	}
	return b.String()
}

func stepStatusIcon(s planner.StepStatus) string {
	switch s {
	case planner.StepCompleted:
		return "✅"
	case planner.StepFailed:
		return "❌"
	case planner.StepSkipped:
		return "⏭"
	case planner.StepRunning:
		return "⏳"
	default:
		return "•"
	}
}

func truncateLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// executePlanStep is the StepExecutor wired into the planner. It translates a
// plan Step into a ToolCall and routes it through the orchestrator's existing
// executeTool dispatch. This keeps tool permission checks, metrics,
// sensitive-pattern detection etc. in one place.
func (o *Orchestrator) executePlanStep(ctx context.Context, step planner.Step) (string, error) {
	// The Planner's Step.Parameters already maps to tool arguments. When a
	// step has no parameters we pass an empty JSON object so downstream
	// decoders don't blow up on nil.
	args := step.Parameters
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	tc := models.ToolCall{
		ID:   step.ID,
		Name: step.Action, // Plan step Action == tool name by convention
		Args: args,
	}
	result, err := o.executeTool(ctx, tc)
	if err != nil {
		return "", err
	}
	if result != nil && result.IsError {
		return result.Content, fmt.Errorf("tool reported error: %s", truncateLine(result.Content, 200))
	}
	if result == nil {
		return "", nil
	}
	return result.Content, nil
}

// persistPlan serializes a plan to JSON and saves it via the store.
// Failures are logged but not propagated — plan persistence is best-effort.
func (o *Orchestrator) persistPlan(ctx context.Context, taskID string, plan *planner.Plan) {
	if o.store == nil || plan == nil {
		return
	}
	data, err := json.Marshal(plan)
	if err != nil {
		o.logger.Warn("failed to marshal plan for persistence", zap.Error(err))
		return
	}
	if err := o.store.SavePlan(ctx, taskID, data); err != nil {
		o.logger.Warn("failed to persist plan", zap.String("task_id", taskID), zap.Error(err))
	}
}
