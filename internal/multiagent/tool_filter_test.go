package multiagent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

func TestFilteredToolExecutor_AllowedTool(t *testing.T) {
	inner := &mockToolExecutor{result: &models.ToolResult{Content: "ok"}}
	filtered := NewFilteredToolExecutor(inner, []string{models.ToolReadFile, models.ToolWriteFile})

	result, err := filtered.Execute(context.Background(), models.ToolCall{
		ID: "tc1", Name: models.ToolReadFile, Args: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "ok" {
		t.Fatalf("expected 'ok', got %q", result.Content)
	}
}

func TestFilteredToolExecutor_DisallowedTool(t *testing.T) {
	inner := &mockToolExecutor{result: &models.ToolResult{Content: "ok"}}
	filtered := NewFilteredToolExecutor(inner, []string{models.ToolReadFile})

	result, err := filtered.Execute(context.Background(), models.ToolCall{
		ID: "tc1", Name: models.ToolWriteFile, Args: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal("expected nil error (error in result)")
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for disallowed tool")
	}
}

func TestFilteredToolProvider_FiltersDefinitions(t *testing.T) {
	inner := &mockToolProvider{defs: []models.ToolDefinition{
		{Name: models.ToolReadFile}, {Name: models.ToolWriteFile}, {Name: models.ToolDeleteFile},
	}}
	filtered := NewFilteredToolProvider(inner, []string{models.ToolReadFile, models.ToolWriteFile})

	defs := filtered.Definitions()
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
	for _, d := range defs {
		if d.Name != models.ToolReadFile && d.Name != models.ToolWriteFile {
			t.Fatalf("unexpected tool: %s", d.Name)
		}
	}
}

type mockToolExecutor struct {
	result *models.ToolResult
}

func (m *mockToolExecutor) Execute(_ context.Context, tc models.ToolCall) (*models.ToolResult, error) {
	r := *m.result
	r.ToolCallID = tc.ID
	return &r, nil
}

type mockToolProvider struct {
	defs []models.ToolDefinition
}

func (m *mockToolProvider) Definitions() []models.ToolDefinition { return m.defs }

func TestSubAgent_DirectPath(t *testing.T) {
	agent := NewSubAgent(AgentCode, zap.NewNop())

	dispatcher := &mockDispatcher{}
	output, err := agent.Execute(context.Background(), DelegationRequest{
		StepID: "s1",
		Action: models.ToolReadFile,
		Task:   "read main.go",
	}, dispatcher)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != "ok: read_file" {
		t.Fatalf("expected 'ok: read_file', got %q", output)
	}
}

func TestSubAgent_DirectPath_DisallowedTool(t *testing.T) {
	agent := NewSubAgent(AgentReview, zap.NewNop())

	dispatcher := &mockDispatcher{}
	_, err := agent.Execute(context.Background(), DelegationRequest{
		StepID: "s1",
		Action: models.ToolWriteFile,
		Task:   "write something",
	}, dispatcher)
	if err == nil {
		t.Fatal("expected error for disallowed tool")
	}
}
