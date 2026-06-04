// failure_tracker.go — loop detector for the ReAct inner loop.
// Delegates to agentloop.ConsecutiveFailureTracker.
package orchestrator

import (
	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/models"
)

// track records a tool result and returns true if a "step back" prompt should be injected.
func (t *consecutiveFailureTracker) track(toolName string, isError bool) bool {
	tracker := &agentloop.ConsecutiveFailureTracker{
		LastFailedTool: t.lastFailedTool,
		FailCount:      t.failCount,
	}
	result := tracker.Track(toolName, isError)
	t.lastFailedTool = tracker.LastFailedTool
	t.failCount = tracker.FailCount
	return result
}

// stepBackMessage returns a system message telling the LLM to rethink its approach.
func (t *consecutiveFailureTracker) stepBackMessage() models.Message {
	tracker := &agentloop.ConsecutiveFailureTracker{
		LastFailedTool: t.lastFailedTool,
		FailCount:      t.failCount,
	}
	return tracker.StepBackMessage()
}

// shouldAbort reports whether the same tool has failed past the hard-stop
// threshold and the ReAct loop must bail out (see agentloop.FixLoopAbortThreshold).
func (t *consecutiveFailureTracker) shouldAbort() bool {
	return t.failCount >= agentloop.FixLoopAbortThreshold
}
