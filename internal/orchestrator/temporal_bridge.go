package orchestrator

import (
	"context"
	"fmt"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

type ctxKeySkipHITL struct{}

// ContextWithSkipHITL returns a context that tells ProcessMessage to skip the
// HITL approval check. Used by Temporal ExecuteTaskActivity after the workflow
// has already obtained approval.
func ContextWithSkipHITL(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeySkipHITL{}, true)
}

func skipHITL(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeySkipHITL{}).(bool)
	return v
}

// TemporalClient is the minimal interface the orchestrator needs from a Temporal client.
type TemporalClient interface {
	StartHITLWorkflow(ctx context.Context, taskID, sessionID, userMessage string) (workflowID string, err error)
	SignalApproval(ctx context.Context, workflowID string, approved bool, comment string) error
}

// SetTemporalClient injects an optional Temporal client for durable HITL.
func (o *Orchestrator) SetTemporalClient(tc TemporalClient) {
	o.temporalClient = tc
}

// suspendForApprovalTemporal starts a Temporal workflow for HITL approval.
func (o *Orchestrator) suspendForApprovalTemporal(ctx context.Context, task *models.Task) (*models.ChatResponse, error) {
	wfID, err := o.temporalClient.StartHITLWorkflow(ctx, task.ID, task.SessionID, task.UserInput)
	if err != nil {
		o.logger.Warn("temporal HITL workflow failed to start, falling back to in-process",
			zap.String("task_id", task.ID), zap.Error(err))
		return o.suspendForApprovalInProcess(ctx, task)
	}

	o.logger.Info("HITL workflow started via Temporal",
		zap.String("task_id", task.ID),
		zap.String("workflow_id", wfID))

	approval := &models.ApprovalRequest{
		TaskID:      task.ID,
		SessionID:   task.SessionID,
		Action:      fmt.Sprintf("Execute %s operation", task.Intent),
		RiskLevel:   "high",
		Details:     task.UserInput,
		RequestedAt: task.CreatedAt,
	}

	return &models.ChatResponse{
		SessionID: task.SessionID, TaskID: task.ID,
		State:    models.TaskStateSuspended,
		Message:  "⚠️ This operation requires approval. Please review and confirm.",
		Approval: approval,
	}, nil
}

// HandleApprovalTemporal sends an approval signal to the Temporal workflow.
func (o *Orchestrator) HandleApprovalTemporal(ctx context.Context, resp models.ApprovalResponse) (*models.ChatResponse, error) {
	wfID := "hitl-" + resp.TaskID
	if err := o.temporalClient.SignalApproval(ctx, wfID, resp.Approved, resp.Comment); err != nil {
		return nil, fmt.Errorf("signal temporal workflow: %w", err)
	}

	state := models.TaskStateExecuting
	msg := "Operation approved. Executing via Temporal workflow..."
	if !resp.Approved {
		state = models.TaskStateCancelled
		msg = "Operation cancelled by user."
	}

	return &models.ChatResponse{
		TaskID:  resp.TaskID,
		State:   state,
		Message: msg,
	}, nil
}
