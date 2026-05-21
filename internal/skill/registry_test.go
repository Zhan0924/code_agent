package skill

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

func newTestDef(name string) *Definition {
	return &Definition{
		Name:        name,
		Description: "A test skill",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Executor: ExecutorConfig{
			Type:   "function",
			Method: "POST",
		},
	}
}

func TestRegistryRegisterAndList(t *testing.T) {
	r := NewRegistry(zap.NewNop())

	def := newTestDef("test_tool")
	if err := r.Register(def); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Duplicate should fail
	if err := r.Register(newTestDef("test_tool")); err == nil {
		t.Fatal("expected error on duplicate register")
	}

	list := r.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(list))
	}
	if list[0].Name != "test_tool" || list[0].Status != "active" {
		t.Fatalf("unexpected status: %+v", list[0])
	}
}

func TestRegistryFindSkill(t *testing.T) {
	r := NewRegistry(zap.NewNop())
	_ = r.Register(newTestDef("my_skill"))

	if _, ok := r.FindSkill("my_skill"); !ok {
		t.Fatal("expected to find my_skill")
	}
	if _, ok := r.FindSkill("nonexistent"); ok {
		t.Fatal("expected not to find nonexistent")
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := NewRegistry(zap.NewNop())
	_ = r.Register(newTestDef("removeme"))

	if err := r.Unregister("removeme"); err != nil {
		t.Fatalf("Unregister failed: %v", err)
	}
	if err := r.Unregister("removeme"); err == nil {
		t.Fatal("expected error on double unregister")
	}
	if len(r.List()) != 0 {
		t.Fatal("expected empty list after unregister")
	}
}

func TestRegistryGetToolDefinitions(t *testing.T) {
	r := NewRegistry(zap.NewNop())
	_ = r.Register(newTestDef("shell_tool"))

	tools := r.GetToolDefinitions()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool def, got %d", len(tools))
	}
	if tools[0].Source == "" {
		t.Fatal("expected non-empty source")
	}
}

func TestRegistryExecuteFunction(t *testing.T) {
	r := NewRegistry(zap.NewNop())

	// Register a function skill
	def := newTestDef("echo_fn")
	def.Executor.Type = "function"
	_ = r.Register(def)

	// Register the Go function handler
	r.RegisterFunction("echo_fn", func(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
		return &models.ToolResult{Content: "hello from function"}, nil
	})

	result, err := r.Execute(context.Background(), "echo_fn", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content)
	}
	if result.Content != "hello from function" {
		t.Fatalf("expected 'hello from function', got '%s'", result.Content)
	}
}

func TestRegistryExecuteWebhook(t *testing.T) {
	// Start a test HTTP server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":"webhook response","isError":false}`))
	}))
	defer ts.Close()

	r := NewRegistry(zap.NewNop())
	def := &Definition{
		Name:        "webhook_tool",
		Description: "A webhook skill",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Executor: ExecutorConfig{
			Type:   "webhook",
			URL:    ts.URL,
			Method: "POST",
		},
	}
	if err := r.Register(def); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	result, err := r.Execute(context.Background(), "webhook_tool", json.RawMessage(`{"key":"value"}`))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestRegistryExecuteNotFound(t *testing.T) {
	r := NewRegistry(zap.NewNop())

	_, err := r.Execute(context.Background(), "ghost", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for unknown skill")
	}
}
