package orchestrator

import (
	"testing"
	"time"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

func TestSpeculativeCache_HitMiss(t *testing.T) {
	c := NewSpeculativeToolCache(time.Second, zap.NewNop())
	args := []byte(`{"path":"main.go"}`)

	if _, ok := c.Get("s1", "read_file", args); ok {
		t.Fatalf("expected miss on empty cache")
	}
	res := &models.ToolResult{Content: "file content", IsError: false}
	if !c.Put("s1", "read_file", args, res) {
		t.Fatalf("Put should succeed for idempotent tool")
	}
	got, ok := c.Get("s1", "read_file", args)
	if !ok || got.Content != "file content" {
		t.Fatalf("expected hit, got ok=%v res=%+v", ok, got)
	}
}

func TestSpeculativeCache_SessionIsolation(t *testing.T) {
	c := NewSpeculativeToolCache(time.Second, zap.NewNop())
	args := []byte(`{"path":"main.go"}`)
	c.Put("s1", "read_file", args, &models.ToolResult{Content: "A"})
	if _, ok := c.Get("s2", "read_file", args); ok {
		t.Fatalf("cross-session cache bleed")
	}
}

func TestSpeculativeCache_NonIdempotentBypass(t *testing.T) {
	c := NewSpeculativeToolCache(time.Second, zap.NewNop())
	args := []byte(`{}`)
	ok := c.Put("s1", "edit_file", args, &models.ToolResult{Content: "ok"})
	if ok {
		t.Fatalf("non-idempotent put should be rejected")
	}
	if _, hit := c.Get("s1", "edit_file", args); hit {
		t.Fatalf("non-idempotent must not cache")
	}
}

func TestSpeculativeCache_ErrorNotCached(t *testing.T) {
	c := NewSpeculativeToolCache(time.Second, zap.NewNop())
	args := []byte(`{}`)
	ok := c.Put("s", "read_file", args, &models.ToolResult{Content: "fail", IsError: true})
	if ok {
		t.Fatalf("error results must not be cached")
	}
}

func TestSpeculativeCache_TTL(t *testing.T) {
	c := NewSpeculativeToolCache(30*time.Millisecond, zap.NewNop())
	args := []byte(`{}`)
	c.Put("s", "read_file", args, &models.ToolResult{Content: "v"})
	if _, ok := c.Get("s", "read_file", args); !ok {
		t.Fatalf("should hit right after put")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Get("s", "read_file", args); ok {
		t.Fatalf("should miss after TTL expiry")
	}
}

func TestSpeculativeCache_Invalidate(t *testing.T) {
	c := NewSpeculativeToolCache(time.Second, zap.NewNop())
	args := []byte(`{}`)
	c.Put("s", "read_file", args, &models.ToolResult{Content: "v"})
	c.Invalidate("s")
	if _, ok := c.Get("s", "read_file", args); ok {
		t.Fatalf("invalidate failed")
	}
}

func TestShouldInvalidateAfter(t *testing.T) {
	cases := map[string]bool{
		"read_file":   false,
		"edit_file":   true,
		"run_sandbox": true,
		"git_commit":  true,
		"grep":        false,
	}
	for name, want := range cases {
		if got := ShouldInvalidateAfter(name); got != want {
			t.Fatalf("ShouldInvalidateAfter(%s)=%v want %v", name, got, want)
		}
	}
}

// TestSpeculativeCache_SharedScopeSeesInvalidation documents the intended
// cross-session semantics: when two sessions share the same cache scope (the
// workspace ID), a write in one session's scope must invalidate reads in the
// other. Previously the cache was keyed by sessionID alone, so session B
// could serve stale reads after session A wrote. The fix is to use the
// workspace ID as the scope; this test encodes that contract.
func TestSpeculativeCache_SharedScopeSeesInvalidation(t *testing.T) {
	c := NewSpeculativeToolCache(time.Second, zap.NewNop())
	args := []byte(`{"path":"main.go"}`)
	const workspaceScope = "ws-42"

	// Session A populates a read-file cache under the workspace scope.
	c.Put(workspaceScope, "read_file", args, &models.ToolResult{Content: "v1"})

	// Session B (same workspace) reads with the SAME scope — should hit.
	got, ok := c.Get(workspaceScope, "read_file", args)
	if !ok || got.Content != "v1" {
		t.Fatalf("expected cross-session hit via shared scope, got ok=%v res=%+v", ok, got)
	}

	// Session A writes to the file (invalidates by workspace scope).
	c.Invalidate(workspaceScope)

	// Session B's read must now miss — no stale-read regression.
	if _, ok := c.Get(workspaceScope, "read_file", args); ok {
		t.Fatal("expected miss after invalidate; otherwise session B sees stale content")
	}
}
