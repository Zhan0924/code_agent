package agentloop

import (
	"fmt"

	"github.com/agent/code_agent/internal/models"
)

// ConsecutiveFailureTracker detects fix loops where the same tool fails repeatedly.
type ConsecutiveFailureTracker struct {
	LastFailedTool string
	FailCount      int
}

// FixLoopWarnThreshold is the failure count at which a "step back" prompt is
// injected to encourage the LLM to try a different approach.
const FixLoopWarnThreshold = 3

// FixLoopAbortThreshold is the failure count at which the ReAct loop should
// hard-stop. This guards against the LLM ignoring the step-back prompt and
// continuing to call the same broken tool indefinitely (e.g. hallucinated
// tool names like "shell_exec").
const FixLoopAbortThreshold = 5

// Track records a tool result and returns true if a "step back" prompt should
// be injected (3+ consecutive failures of the same tool).
func (t *ConsecutiveFailureTracker) Track(toolName string, isError bool) bool {
	if !isError {
		t.LastFailedTool = ""
		t.FailCount = 0
		return false
	}
	if toolName == t.LastFailedTool {
		t.FailCount++
	} else {
		t.LastFailedTool = toolName
		t.FailCount = 1
	}
	return t.FailCount >= FixLoopWarnThreshold
}

// ShouldAbort returns true once the same tool has failed FixLoopAbortThreshold
// or more times in a row — at that point the ReAct loop is provably stuck and
// the orchestrator should bail out instead of burning more steps and tokens.
func (t *ConsecutiveFailureTracker) ShouldAbort() bool {
	return t.FailCount >= FixLoopAbortThreshold
}

// StepBackMessage returns a system message telling the LLM to rethink its approach.
func (t *ConsecutiveFailureTracker) StepBackMessage() models.Message {
	return models.Message{
		Role: models.RoleSystem,
		Content: fmt.Sprintf(
			"[⚠️ FIX LOOP DETECTED — '%s' has failed %d consecutive times]\n"+
				"STOP repeating the same approach. Step back and:\n"+
				"1. Re-read the error messages carefully — what is the ROOT CAUSE?\n"+
				"2. Read the relevant source file to understand the current state\n"+
				"3. Consider a DIFFERENT fix approach (e.g., if you've been patching, try rewriting the function)\n"+
				"4. If the error is in a dependency, check go.mod or imports first\n"+
				"Do NOT run the same failing command again until you've made a meaningful code change.",
			t.LastFailedTool, t.FailCount),
	}
}
