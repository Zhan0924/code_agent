package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

func TestCanParallelExecute(t *testing.T) {
	tests := []struct {
		name     string
		calls    []models.ToolCall
		expected bool
	}{
		{"single call", []models.ToolCall{{Name: "read_file"}}, false},
		{"empty", nil, false},
		{"all idempotent", []models.ToolCall{{Name: "read_file"}, {Name: "grep"}, {Name: "list_dir"}}, true},
		{"mixed", []models.ToolCall{{Name: "read_file"}, {Name: "write_file"}}, false},
		{"all write", []models.ToolCall{{Name: "edit_file"}, {Name: "write_file"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canParallelExecute(tt.calls); got != tt.expected {
				t.Errorf("canParallelExecute() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestParallelExecuteTools_PreCancelledCtx_ReturnsPromptly pins the fix for
// the bare wg.Wait race. Before the fix, parallelExecuteTools blocked on
// wg.Wait even after the caller cancelled ctx — a hung read_file or RAG
// retrieval inside executeTool would freeze the ReAct loop indefinitely.
//
// We can't easily make executeTool block in unit tests (no fake LLM scaffold
// here), but the regression we care about is the wg.Wait-vs-ctx.Done race.
// Strategy: pre-cancel ctx, send a small batch of "unknown path" reads so
// executeTool returns quickly through its error path, and assert that the
// call returns within a hard deadline rather than hanging.
func TestParallelExecuteTools_PreCancelledCtx_ReturnsPromptly(t *testing.T) {
	o := &Orchestrator{logger: zap.NewNop()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := []models.ToolCall{
		{ID: "1", Name: "read_file", Args: []byte(`{"path":"/nope/a"}`)},
		{ID: "2", Name: "read_file", Args: []byte(`{"path":"/nope/b"}`)},
	}

	done := make(chan []toolExecResult, 1)
	go func() {
		done <- o.parallelExecuteTools(ctx, calls)
	}()

	select {
	case results := <-done:
		if len(results) != len(calls) {
			t.Errorf("expected %d result slots, got %d", len(calls), len(results))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parallelExecuteTools did not return within 5s after ctx cancel; wg.Wait race regressed")
	}
}

// TestParallelExecuteTools_CtxCancelDuringExecution exercises the race
// between worker completion and ctx cancellation. The exact result content
// is not asserted — workers may observe the cancel (clean error) or have
// already completed (clean result); either is correct. The regression we
// guard against is a deadlock.
func TestParallelExecuteTools_CtxCancelDuringExecution(t *testing.T) {
	o := &Orchestrator{logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := []models.ToolCall{
		{ID: "1", Name: "read_file", Args: []byte(`{"path":"/nope/a"}`)},
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan []toolExecResult, 1)
	go func() {
		done <- o.parallelExecuteTools(ctx, calls)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parallelExecuteTools did not return within 5s of ctx cancel")
	}
}
