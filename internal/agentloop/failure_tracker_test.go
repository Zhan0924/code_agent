package agentloop

import "testing"

// TestConsecutiveFailureTracker_WarnThenAbort verifies the staged behaviour:
// - first warn at FixLoopWarnThreshold (step-back prompt should be injected)
// - hard abort at FixLoopAbortThreshold (ReAct loop must bail)
// - any success resets the counter
func TestConsecutiveFailureTracker_WarnThenAbort(t *testing.T) {
	tr := &ConsecutiveFailureTracker{}

	// 1st and 2nd consecutive errors: no warning yet, no abort.
	for i := 1; i < FixLoopWarnThreshold; i++ {
		if tr.Track("shell_exec", true) {
			t.Fatalf("warn triggered too early at failure %d", i)
		}
		if tr.ShouldAbort() {
			t.Fatalf("abort triggered too early at failure %d", i)
		}
	}

	// Hitting warn threshold (3rd) should warn, but not yet abort.
	if !tr.Track("shell_exec", true) {
		t.Fatalf("expected warn at threshold %d", FixLoopWarnThreshold)
	}
	if tr.ShouldAbort() {
		t.Fatalf("abort triggered at warn threshold (failures=%d)", tr.FailCount)
	}

	// Push to abort threshold.
	for tr.FailCount < FixLoopAbortThreshold {
		tr.Track("shell_exec", true)
	}
	if !tr.ShouldAbort() {
		t.Fatalf("expected abort at failures=%d (threshold=%d)", tr.FailCount, FixLoopAbortThreshold)
	}

	// A success in between must fully reset state.
	tr.Track("write_file", false)
	if tr.ShouldAbort() {
		t.Fatalf("abort still set after success reset (state=%+v)", tr)
	}
	if tr.FailCount != 0 || tr.LastFailedTool != "" {
		t.Errorf("expected reset, got %+v", tr)
	}
}

// TestConsecutiveFailureTracker_DifferentToolResets verifies that switching to
// a different failing tool restarts the counter — the loop detector is
// per-tool, not per-step.
func TestConsecutiveFailureTracker_DifferentToolResets(t *testing.T) {
	tr := &ConsecutiveFailureTracker{}
	tr.Track("shell_exec", true)
	tr.Track("shell_exec", true)
	tr.Track("run_workspace_cmd", true) // different tool → counter resets to 1
	if tr.FailCount != 1 {
		t.Errorf("FailCount = %d, want 1 after tool switch", tr.FailCount)
	}
	if tr.LastFailedTool != "run_workspace_cmd" {
		t.Errorf("LastFailedTool = %q, want run_workspace_cmd", tr.LastFailedTool)
	}
}
