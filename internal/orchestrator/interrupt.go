package orchestrator

import (
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

// Note: the former checkInterrupt / formatInterruptResponse / interruptAction
// were dead code. The real interrupt handling happens inline in
// react_core.go's step-boundary select (see reactLoopCore "Check interrupt"),
// where the ToolTransaction Rollback is also triggered.
