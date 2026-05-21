package orchestrator

import (
	"context"
	"fmt"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// InterruptType defines the kind of interrupt signal.
type InterruptType string

const (
	InterruptCancel   InterruptType = "cancel"
	InterruptRedirect InterruptType = "redirect"
	InterruptPause    InterruptType = "pause"
)

// InterruptSignal carries the interrupt intent from the API layer to the ReAct loop.
type InterruptSignal struct {
	Type       InterruptType `json:"type"`
	NewMessage string        `json:"new_message,omitempty"`
}

// InterruptSession sends an interrupt signal to a running session's ReAct loop.
// Returns false if no active loop is listening on that session.
func (o *Orchestrator) InterruptSession(sessionID string, signal InterruptSignal) bool {
	o.interruptMu.RLock()
	ch, ok := o.interruptCh[sessionID]
	o.interruptMu.RUnlock()
	if !ok {
		return false
	}
	select {
	case ch <- signal:
		o.logger.Info("interrupt signal sent",
			zap.String("session_id", sessionID),
			zap.String("type", string(signal.Type)))
		return true
	default:
		return false
	}
}

// registerInterrupt creates an interrupt channel for a session before entering the ReAct loop.
func (o *Orchestrator) registerInterrupt(sessionID string) chan InterruptSignal {
	ch := make(chan InterruptSignal, 1)
	o.interruptMu.Lock()
	o.interruptCh[sessionID] = ch
	o.interruptMu.Unlock()
	return ch
}

// unregisterInterrupt removes the interrupt channel after the ReAct loop exits.
func (o *Orchestrator) unregisterInterrupt(sessionID string) {
	o.interruptMu.Lock()
	delete(o.interruptCh, sessionID)
	o.interruptMu.Unlock()
}

// checkInterrupt is called between tool executions in the ReAct loop.
// Returns a non-nil response if the loop should exit early.
func (o *Orchestrator) checkInterrupt(ctx context.Context, ch chan InterruptSignal, task *models.Task) (*interruptAction, bool) {
	select {
	case sig := <-ch:
		switch sig.Type {
		case InterruptCancel:
			return &interruptAction{
				response: "Task cancelled by user.",
				cancel:   true,
			}, true
		case InterruptRedirect:
			return &interruptAction{
				response:   "",
				redirect:   true,
				newMessage: sig.NewMessage,
			}, true
		case InterruptPause:
			return &interruptAction{
				response: "Task paused by user. Send 'continue' to resume.",
				cancel:   true,
			}, true
		}
	default:
	}
	_ = ctx
	return nil, false
}

type interruptAction struct {
	response   string
	cancel     bool
	redirect   bool
	newMessage string
}

// formatInterruptResponse builds the ChatResponse for an interrupted task.
func (o *Orchestrator) formatInterruptResponse(task *models.Task, action *interruptAction) *models.ChatResponse {
	msg := action.response
	if action.redirect {
		msg = fmt.Sprintf("Task redirected. Processing new request: %s", action.newMessage)
	}
	return &models.ChatResponse{
		SessionID: task.SessionID,
		TaskID:    task.ID,
		Message:   msg,
		State:     models.TaskStateFailed,
	}
}
