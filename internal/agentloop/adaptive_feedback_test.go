package agentloop

import (
	"strings"
	"testing"
)

func TestAdaptiveFeedback_FirstTime(t *testing.T) {
	af := &AdaptiveFeedback{}
	te := &ToolError{Category: ErrCatNotFound, ToolName: "read_file", Message: "no such file: foo.go"}
	af.Record(te)
	fb := af.BuildFeedback(te)
	if !strings.Contains(fb, "资源不存在") {
		t.Errorf("expected not-found feedback, got: %s", fb)
	}
}

func TestAdaptiveFeedback_Repeat(t *testing.T) {
	af := &AdaptiveFeedback{}
	te := &ToolError{Category: ErrCatInvalidArgs, ToolName: "edit_file", Message: "invalid path"}

	af.Record(te)
	af.Record(te)
	fb := af.BuildFeedback(te)
	if !strings.Contains(fb, "换用不同参数或换工具") {
		t.Errorf("expected repeat feedback, got: %s", fb)
	}
}

func TestAdaptiveFeedback_SlidingWindow(t *testing.T) {
	af := &AdaptiveFeedback{}
	for i := range 15 {
		te := &ToolError{Category: ErrCatExecFailed, ToolName: "run_command", Message: strings.Repeat("x", i)}
		af.Record(te)
	}
	if len(af.history) != maxErrorHistory {
		t.Errorf("expected history capped at %d, got %d", maxErrorHistory, len(af.history))
	}
}

func TestAdaptiveFeedback_Permission(t *testing.T) {
	af := &AdaptiveFeedback{}
	te := &ToolError{Category: ErrCatPermission, ToolName: "read_file", Message: "access denied"}
	af.Record(te)
	fb := af.BuildFeedback(te)
	if !strings.Contains(fb, "权限不足") {
		t.Errorf("expected permission feedback, got: %s", fb)
	}
}

func TestAdaptiveFeedback_TimeoutRepeat(t *testing.T) {
	af := &AdaptiveFeedback{}
	te := &ToolError{Category: ErrCatTimeout, ToolName: "run_command", Message: "timeout"}
	af.Record(te)
	af.Record(te)
	fb := af.BuildFeedback(te)
	if !strings.Contains(fb, "多次超时") {
		t.Errorf("expected timeout repeat feedback, got: %s", fb)
	}
}

func TestAdaptiveFeedback_Blacklist(t *testing.T) {
	af := &AdaptiveFeedback{}
	te := &ToolError{Category: ErrCatNotFound, ToolName: "read_file", Message: "not found"}

	for range blacklistThreshold {
		af.Record(te)
	}

	if !af.IsBlacklisted("read_file") {
		t.Error("expected read_file to be blacklisted after threshold failures")
	}

	fb := af.BuildFeedback(te)
	if !strings.Contains(fb, "list_dir") && !strings.Contains(fb, "grep") {
		t.Errorf("expected alternative suggestion, got: %s", fb)
	}
}

func TestAdaptiveFeedback_BlacklistReset(t *testing.T) {
	af := &AdaptiveFeedback{}
	te := &ToolError{Category: ErrCatExecFailed, ToolName: "execute_code", Message: "failed"}

	for range blacklistThreshold {
		af.Record(te)
	}
	if !af.IsBlacklisted("execute_code") {
		t.Fatal("should be blacklisted")
	}

	af.RecordSuccess("execute_code")
	if af.IsBlacklisted("execute_code") {
		t.Error("blacklist should reset after success")
	}
}
