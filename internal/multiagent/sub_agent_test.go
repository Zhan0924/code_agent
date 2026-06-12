package multiagent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// mockLLMCaller returns pre-configured responses.
type mockLLMCaller struct {
	responses []*llm.ChatResponse
	callIdx   int
}

func (m *mockLLMCaller) ChatCompletion(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	idx := m.callIdx
	m.callIdx++
	if idx >= len(m.responses) {
		return &llm.ChatResponse{Content: "done"}, nil
	}
	return m.responses[idx], nil
}

func TestSubAgent_ReActPath_MultiStep(t *testing.T) {
	ml := &mockLLMCaller{responses: []*llm.ChatResponse{
		{Content: "Reading file first.", ToolCalls: []models.ToolCall{
			{ID: "tc1", Name: models.ToolReadFile, Args: json.RawMessage(`{"path":"main.go"}`)},
		}},
		{Content: "Now editing.", ToolCalls: []models.ToolCall{
			{ID: "tc2", Name: models.ToolEditFile, Args: json.RawMessage(`{"path":"main.go"}`)},
		}},
		{Content: "Task complete. File has been updated."},
	}}

	te := &mockToolExecutor{result: &models.ToolResult{Content: "file content"}}
	tp := &mockToolProvider{defs: []models.ToolDefinition{
		{Name: models.ToolReadFile}, {Name: models.ToolEditFile}, {Name: models.ToolWriteFile},
	}}

	agent := NewSubAgent(AgentCode, zap.NewNop())
	deps := &AgentDeps{
		LLM:          ml,
		ToolExecutor: te,
		ToolProvider: tp,
		EventSink:    agentloop.NoopSink{},
	}

	output, err := agent.ExecuteWithDeps(context.Background(), DelegationRequest{
		StepID:            "s1",
		Task:              "Update main.go to add error handling",
		ReasoningRequired: true,
	}, nil, deps)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != "Task complete. File has been updated." {
		t.Fatalf("unexpected output: %q", output)
	}
	if ml.callIdx != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", ml.callIdx)
	}
}

func TestSubAgent_ReActPath_StepLimitReturnsError(t *testing.T) {
	// LLM always returns tool calls, never finishes
	responses := make([]*llm.ChatResponse, 20)
	for i := range responses {
		responses[i] = &llm.ChatResponse{
			Content:   "still working",
			ToolCalls: []models.ToolCall{{ID: "tc", Name: models.ToolReadFile, Args: json.RawMessage(`{}`)}},
		}
	}
	ml := &mockLLMCaller{responses: responses}
	te := &mockToolExecutor{result: &models.ToolResult{Content: "ok"}}
	tp := &mockToolProvider{defs: []models.ToolDefinition{{Name: models.ToolReadFile}}}

	agent := NewSubAgent(AgentCode, zap.NewNop())
	deps := &AgentDeps{
		LLM:          ml,
		ToolExecutor: te,
		ToolProvider: tp,
	}

	_, err := agent.ExecuteWithDeps(context.Background(), DelegationRequest{
		StepID:            "s1",
		Task:              "infinite task",
		ReasoningRequired: true,
	}, nil, deps)

	if err == nil {
		t.Fatal("expected error for step limit")
	}
}

func TestSubAgent_FallsBackToDirectWhenNoDeps(t *testing.T) {
	agent := NewSubAgent(AgentCode, zap.NewNop())
	dispatcher := &mockDispatcher{}

	// ReasoningRequired=true but deps=nil → should fall back to direct
	output, err := agent.ExecuteWithDeps(context.Background(), DelegationRequest{
		StepID:            "s1",
		Action:            models.ToolReadFile,
		Task:              "read file",
		ReasoningRequired: true,
	}, dispatcher, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != "ok: read_file" {
		t.Fatalf("expected 'ok: read_file', got %q", output)
	}
}
