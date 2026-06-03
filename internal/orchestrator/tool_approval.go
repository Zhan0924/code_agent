package orchestrator

// tool_approval.go — Tool-level HITL: wire the [P5] RiskLevel>=2 gate into the
// existing suspendForApproval flow so the SSE stream surfaces an
// approval_request event and the /tasks/:id/approve endpoint resumes the
// blocked tool call instead of returning a dead-end error.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent/code_agent/internal/metrics"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// ctx keys for plumbing the running task and event sink to deeply nested
// callsites (executeTool) without touching every intermediate signature.
type ctxToolApprovalTaskKey struct{}
type ctxToolApprovalSinkKey struct{}

func withToolApprovalContext(ctx context.Context, task *models.Task, sink reactEventSink) context.Context {
	if task != nil {
		ctx = context.WithValue(ctx, ctxToolApprovalTaskKey{}, task)
	}
	if sink != nil {
		ctx = context.WithValue(ctx, ctxToolApprovalSinkKey{}, sink)
	}
	return ctx
}

func taskFromCtx(ctx context.Context) *models.Task {
	v, _ := ctx.Value(ctxToolApprovalTaskKey{}).(*models.Task)
	return v
}

func sinkFromCtx(ctx context.Context) reactEventSink {
	v, _ := ctx.Value(ctxToolApprovalSinkKey{}).(reactEventSink)
	return v
}

// toolApprovalTimeout caps how long executeTool blocks waiting for the user.
// Shorter than the task-level 30 min — tool-level prompts are inline in the
// chat and the user is usually right there.
const toolApprovalTimeout = 5 * time.Minute

// riskLabel maps numeric RiskLevel to the string the frontend already renders.
func riskLabel(level int) string {
	switch {
	case level >= 3:
		return "critical"
	case level == 2:
		return "high"
	case level == 1:
		return "medium"
	default:
		return "low"
	}
}

// waitToolApproval emits an approval_request event on the sink, blocks until
// the user submits a decision via HandleApproval (which routes to
// toolApprovalCh) or the timeout fires.
//
// Returns (approved, err). err is non-nil only on timeout / missing channel
// infrastructure; in that case the caller should treat the tool call as
// rejected so the ReAct loop doesn't silently advance.
func (o *Orchestrator) waitToolApproval(
	ctx context.Context,
	task *models.Task,
	tc models.ToolCall,
	def models.ToolDefinition,
	sink reactEventSink,
) (bool, error) {
	ch := make(chan models.ApprovalResponse, 1)
	o.toolApprovalMu.Lock()
	// Same task can't have two pending tool approvals — the ReAct loop is
	// serial. If one is already pending, fail closed.
	if _, dup := o.toolApprovalCh[task.ID]; dup {
		o.toolApprovalMu.Unlock()
		return false, fmt.Errorf("another tool approval already pending for task %s", task.ID)
	}
	o.toolApprovalCh[task.ID] = ch
	o.toolApprovalMu.Unlock()
	defer func() {
		o.toolApprovalMu.Lock()
		delete(o.toolApprovalCh, task.ID)
		o.toolApprovalMu.Unlock()
	}()

	metrics.HITLPendingGauge.Inc()
	defer metrics.HITLPendingGauge.Dec()

	// Best-effort args preview — used by the frontend modal for details.
	argsPreview := string(tc.Args)
	if len(argsPreview) > 2000 {
		argsPreview = argsPreview[:2000] + "... (truncated)"
	}

	rl := def.RiskLevel
	metadata := map[string]interface{}{
		"task_id":      task.ID,
		"session_id":   task.SessionID,
		"action":       fmt.Sprintf("Execute %s", tc.Name),
		"risk_level":   riskLabel(rl),
		"details":      argsPreview,
		"tool_name":    tc.Name,
		"tool_call_id": tc.ID,
		"requested_at": time.Now().UTC().Format(time.RFC3339),
	}
	metaJSON, _ := json.Marshal(metadata)
	sink.Emit(models.ReactStreamEvent{
		Type:       "approval_request",
		Content:    fmt.Sprintf("Tool '%s' requires approval (risk_level=%d)", tc.Name, rl),
		ToolName:   tc.Name,
		ToolCallID: tc.ID,
		ToolArgs:   argsPreview,
		TaskID:     task.ID,
		Metadata:   json.RawMessage(metaJSON),
	})

	o.logger.Info("tool waiting for human approval",
		zap.String("task_id", task.ID),
		zap.String("tool", tc.Name),
		zap.Int("risk_level", rl))

	waitCtx, cancel := context.WithTimeout(ctx, toolApprovalTimeout)
	defer cancel()

	select {
	case resp := <-ch:
		if resp.Approved {
			metrics.HITLApprovalTotal.WithLabelValues("approved").Inc()
		} else {
			metrics.HITLApprovalTotal.WithLabelValues("rejected").Inc()
		}
		return resp.Approved, nil
	case <-waitCtx.Done():
		metrics.HITLApprovalTotal.WithLabelValues("timeout").Inc()
		return false, fmt.Errorf("approval timed out after %s", toolApprovalTimeout)
	}
}
