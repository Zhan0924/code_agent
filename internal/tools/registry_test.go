package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agent/code_agent/internal/models"
)

// ─── Test fixtures ──────────────────────────────────────────────────────────

type stubTool struct {
	def    models.ToolDefinition
	result *models.ToolResult
	err    error
}

func (s *stubTool) Definition() models.ToolDefinition { return s.def }
func (s *stubTool) Execute(_ context.Context, _ json.RawMessage) (*models.ToolResult, error) {
	return s.result, s.err
}

func newStub(name, source string) *stubTool {
	return &stubTool{
		def: models.ToolDefinition{
			Name:        name,
			Description: "test tool: " + name,
			Source:      source,
		},
		result: &models.ToolResult{Content: "ok"},
	}
}

type stubProvider struct {
	name  string
	tools []Tool
}

func (p *stubProvider) Name() string  { return p.name }
func (p *stubProvider) Tools() []Tool { return p.tools }

// ─── Tests ──────────────────────────────────────────────────────────────────

func TestRegistry_Register_AndGet(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("foo", "builtin")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d, want 1", r.Len())
	}
	if _, ok := r.Get("foo"); !ok {
		t.Error("Get(foo) should succeed")
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) should return false")
	}
}

func TestRegistry_Register_Nil(t *testing.T) {
	if err := NewRegistry().Register(nil); err == nil {
		t.Error("Register(nil) should error")
	}
}

func TestRegistry_Register_EmptyName(t *testing.T) {
	r := NewRegistry()
	bad := &stubTool{def: models.ToolDefinition{Name: ""}}
	if err := r.Register(bad); err == nil {
		t.Error("Register with empty name should error")
	}
}

func TestRegistry_Register_Duplicate(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newStub("foo", "builtin"))
	if err := r.Register(newStub("foo", "builtin")); err == nil {
		t.Error("duplicate registration should error")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newStub("foo", "builtin"))
	if !r.Unregister("foo") {
		t.Error("Unregister should return true for existing tool")
	}
	if r.Unregister("foo") {
		t.Error("Unregister should return false the second time")
	}
	if r.Len() != 0 {
		t.Errorf("Len = %d, want 0 after unregister", r.Len())
	}
}

func TestRegistry_Definitions_SortedByName(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newStub("gamma", "builtin"))
	_ = r.Register(newStub("alpha", "builtin"))
	_ = r.Register(newStub("beta", "mcp:x"))

	defs := r.Definitions()
	if len(defs) != 3 {
		t.Fatalf("len(defs) = %d, want 3", len(defs))
	}
	if defs[0].Name != "alpha" || defs[1].Name != "beta" || defs[2].Name != "gamma" {
		t.Errorf("definitions not sorted: %v", []string{defs[0].Name, defs[1].Name, defs[2].Name})
	}
}

func TestRegistry_RegisterProvider(t *testing.T) {
	r := NewRegistry()
	p := &stubProvider{
		name: "mcp",
		tools: []Tool{
			newStub("a", "mcp:s1"),
			newStub("b", "mcp:s1"),
		},
	}
	n := r.RegisterProvider(p)
	if n != 2 {
		t.Errorf("RegisterProvider returned %d, want 2", n)
	}

	// Re-registering a provider with overlapping names should skip duplicates
	// but not return an error.
	n = r.RegisterProvider(&stubProvider{
		name:  "mcp",
		tools: []Tool{newStub("a", "mcp:s1"), newStub("c", "mcp:s1")},
	})
	if n != 1 {
		t.Errorf("second RegisterProvider returned %d, want 1 (only 'c' is new)", n)
	}
	if r.Len() != 3 {
		t.Errorf("Len = %d, want 3", r.Len())
	}
}

func TestRegistry_RegisterProvider_Nil(t *testing.T) {
	if NewRegistry().RegisterProvider(nil) != 0 {
		t.Error("RegisterProvider(nil) should return 0")
	}
}

func TestRegistry_Execute_Success(t *testing.T) {
	r := NewRegistry()
	stub := newStub("echo", "builtin")
	stub.result = &models.ToolResult{Content: "hello"}
	_ = r.Register(stub)

	res, err := r.Execute(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if res.Content != "hello" {
		t.Errorf("content = %q, want hello", res.Content)
	}
}

func TestRegistry_Execute_NotFound(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(context.Background(), "missing", nil)
	if !errors.Is(err, ErrToolNotFound) {
		t.Errorf("err = %v, want ErrToolNotFound", err)
	}
}

// TestRegistry_Execute_NotFound_SuggestsAlternatives guards the LLM-self-correct
// path: when the model hallucinates a tool name, the error must include the
// closest registered names so the next turn can recover instead of looping.
func TestRegistry_Execute_NotFound_SuggestsAlternatives(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"run_workspace_cmd", "run_tests", "read_file", "write_file", "git_diff"} {
		_ = r.Register(newStub(name, "builtin"))
	}

	// Hallucinated `shell_exec` — should surface run_workspace_cmd / run_tests
	// (both share the "exec/run" semantic via the substring/token match).
	_, err := r.Execute(context.Background(), "shell_exec", nil)
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("err = %v, want ErrToolNotFound", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "Did you mean") {
		t.Errorf("error should hint at alternatives, got: %s", msg)
	}
	if !strings.Contains(msg, "5 tools registered total") {
		t.Errorf("error should advertise the total registered count, got: %s", msg)
	}
	// Semantic aliases must rank run_workspace_cmd / run_tests ahead of unrelated
	// alphabetical neighbours like git_diff / read_file when the query is shell_exec.
	if !strings.Contains(msg, "run_workspace_cmd") {
		t.Errorf("expected run_workspace_cmd in suggestions for shell_exec, got: %s", msg)
	}
	// And the ordering inside the bracket: run_workspace_cmd must appear before git_diff.
	rwc := strings.Index(msg, "run_workspace_cmd")
	gd := strings.Index(msg, "git_diff")
	if rwc < 0 || (gd >= 0 && rwc > gd) {
		t.Errorf("run_workspace_cmd must rank ahead of git_diff for shell_exec, got: %s", msg)
	}
}

func TestRegistry_Execute_ToolError(t *testing.T) {
	r := NewRegistry()
	stub := newStub("fails", "builtin")
	stub.err = errors.New("boom")
	_ = r.Register(stub)

	_, err := r.Execute(context.Background(), "fails", nil)
	if err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestRegistry_Execute_ToolReturnsIsError(t *testing.T) {
	r := NewRegistry()
	stub := newStub("warns", "builtin")
	stub.result = &models.ToolResult{Content: "something wrong", IsError: true}
	_ = r.Register(stub)

	res, err := r.Execute(context.Background(), "warns", nil)
	if err != nil {
		t.Fatalf("Execute err = %v", err)
	}
	if !res.IsError {
		t.Error("result.IsError should be propagated")
	}
}

func TestRegistry_ConcurrentSafe(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newStub("echo", "builtin"))

	done := make(chan struct{})
	// concurrent readers
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = r.Definitions()
				_, _ = r.Get("echo")
			}
			done <- struct{}{}
		}()
	}
	// concurrent writer (add/remove)
	go func() {
		for j := 0; j < 100; j++ {
			_ = r.Register(newStub("t", "builtin"))
			r.Unregister("t")
		}
		done <- struct{}{}
	}()

	for i := 0; i < 11; i++ {
		<-done
	}
}
