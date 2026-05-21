package main

import (
	"context"
	"fmt"

	temporalpkg "github.com/agent/code_agent/internal/temporal"
	temporalclient "go.temporal.io/sdk/client"
)

// temporalHITLAdapter wraps a Temporal SDK client to implement orchestrator.TemporalClient.
type temporalHITLAdapter struct {
	client temporalclient.Client
	queue  string
}

func (a *temporalHITLAdapter) StartHITLWorkflow(ctx context.Context, taskID, sessionID, userMessage string) (string, error) {
	workflowID := "hitl-" + taskID
	opts := temporalclient.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: a.queue,
	}

	input := temporalpkg.AgentTaskInput{
		SessionID:   sessionID,
		UserMessage: userMessage,
		TaskID:      taskID,
	}

	we, err := a.client.ExecuteWorkflow(ctx, opts, temporalpkg.AgentTaskWorkflow, input)
	if err != nil {
		return "", fmt.Errorf("start HITL workflow: %w", err)
	}
	return we.GetID(), nil
}

func (a *temporalHITLAdapter) SignalApproval(ctx context.Context, workflowID string, approved bool, comment string) error {
	return a.client.SignalWorkflow(ctx, workflowID, "", temporalpkg.ApprovalSignal, struct {
		Approved bool   `json:"approved"`
		Comment  string `json:"comment,omitempty"`
	}{
		Approved: approved,
		Comment:  comment,
	})
}
