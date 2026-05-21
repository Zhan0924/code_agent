// failure_tracker.go — loop detector for the ReAct inner loop.
//
// When an agent is iteratively "fixing" a broken change, it is common for it
// to repeatedly call the same tool (e.g. `run_workspace_cmd: go build`) with
// different patches and never break out of the cycle. Tracking consecutive
// identical failures lets us inject a "step back" prompt that forces the LLM
// to re-read context and rethink the approach, rather than brute-forcing.
//
// Extracted from orchestrator.go as part of the file-split refactor. The
// behaviour is intentionally unchanged.
package orchestrator

import (
	"fmt"

	"github.com/agent/code_agent/internal/models"
)

// track records a tool result and returns true if a "step back" prompt should
// be injected. A success clears any pending failure streak.
func (t *consecutiveFailureTracker) track(toolName string, isError bool) bool {
	if !isError {
		t.lastFailedTool = ""
		t.failCount = 0
		return false
	}
	if toolName == t.lastFailedTool {
		t.failCount++
	} else {
		t.lastFailedTool = toolName
		t.failCount = 1
	}
	return t.failCount >= 3
}

// stepBackMessage returns a system message telling the LLM to rethink its approach.
//
// The message is deliberately directive ("STOP repeating", numbered checklist)
// because production LLMs under repeated tool-call failures have a tendency
// to keep retrying superficial tweaks. We want them to break out of that.
func (t *consecutiveFailureTracker) stepBackMessage() models.Message {
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
			t.lastFailedTool, t.failCount),
	}
}
