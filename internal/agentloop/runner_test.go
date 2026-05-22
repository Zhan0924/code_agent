package agentloop

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// mockLLM returns pre-configured responses per call index.
type mockLLM struct {
	responses []*llm.ChatResponse
	errors    []error
	callIdx   int
}

func (m *mockLLM) ChatCompletion(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	idx := m.callIdx
	m.callIdx++
	if idx >= len(m.responses) {
		return &llm.ChatResponse{Content: "done"}, nil
	}
	var err error
	if idx < len(m.errors) {
		err = m.errors[idx]
	}
	return m.responses[idx], err
}

// mockToolExec returns canned tool results.
type mockToolExec struct {
	results map[string]*models.ToolResult
}

func (m *mockToolExec) Execute(_ context.Context, tc models.ToolCall) (*models.ToolResult, error) {
	if r, ok := m.results[tc.Name]; ok {
		return r, nil
	}
	return &models.ToolResult{ToolCallID: tc.ID, Content: "ok"}, nil
}

// mockToolProv provides a fixed set of tool definitions.
type mockToolProv struct {
	tools []models.ToolDefinition
}

func (m *mockToolProv) Definitions() []models.ToolDefinition { return m.tools }

func TestRunner_FinalAnswerFirstStep(t *testing.T) {
	ml := &mockLLM{responses: []*llm.ChatResponse{
		{Content: "The answer is 42."},
	}}
	runner := NewRunner(ml, &mockToolExec{}, &mockToolProv{}, DefaultSubAgentConfig(), zap.NewNop())

	result := runner.Run(context.Background(), RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "You are helpful."}},
		TaskID:   "test-1",
	}, nil)

	if !result.Done {
		t.Fatal("expected done=true")
	}
	if result.Content != "The answer is 42." {
		t.Fatalf("unexpected content: %q", result.Content)
	}
	if result.StepsUsed != 1 {
		t.Fatalf("expected 1 step, got %d", result.StepsUsed)
	}
}

func TestRunner_MultiStepToolCalls(t *testing.T) {
	ml := &mockLLM{responses: []*llm.ChatResponse{
		{Content: "Let me read the file.", ToolCalls: []models.ToolCall{
			{ID: "tc1", Name: "read_file", Args: json.RawMessage(`{"path":"main.go"}`)},
		}},
		{Content: "Now I'll edit.", ToolCalls: []models.ToolCall{
			{ID: "tc2", Name: "edit_file", Args: json.RawMessage(`{"path":"main.go","content":"new"}`)},
		}},
		{Content: "Done. The file has been updated."},
	}}
	te := &mockToolExec{results: map[string]*models.ToolResult{
		"read_file": {ToolCallID: "tc1", Content: "package main"},
		"edit_file": {ToolCallID: "tc2", Content: "file edited"},
	}}
	tp := &mockToolProv{tools: []models.ToolDefinition{
		{Name: "read_file"}, {Name: "edit_file"},
	}}

	runner := NewRunner(ml, te, tp, DefaultSubAgentConfig(), zap.NewNop())
	result := runner.Run(context.Background(), RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "sys"}},
		TaskID:   "test-2",
	}, nil)

	if !result.Done {
		t.Fatal("expected done=true")
	}
	if result.StepsUsed != 3 {
		t.Fatalf("expected 3 steps, got %d", result.StepsUsed)
	}
	if result.Content != "Done. The file has been updated." {
		t.Fatalf("unexpected content: %q", result.Content)
	}
}

func TestRunner_StepLimitExhausted(t *testing.T) {
	// LLM always returns a tool call, never a final answer
	responses := make([]*llm.ChatResponse, 10)
	for i := range responses {
		responses[i] = &llm.ChatResponse{
			Content:   "thinking...",
			ToolCalls: []models.ToolCall{{ID: "tc", Name: "read_file", Args: json.RawMessage(`{}`)}},
		}
	}
	ml := &mockLLM{responses: responses}
	cfg := Config{MaxSteps: 3, MaxContextTokens: 128000, LLMRetries: 1}
	runner := NewRunner(ml, &mockToolExec{}, &mockToolProv{}, cfg, zap.NewNop())

	result := runner.Run(context.Background(), RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "sys"}},
	}, nil)

	if result.Done {
		t.Fatal("expected done=false (step limit hit)")
	}
	if !result.HitStepLimit {
		t.Fatal("expected HitStepLimit=true")
	}
	if result.StepsUsed != 3 {
		t.Fatalf("expected 3 steps, got %d", result.StepsUsed)
	}
}

func TestRunner_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	ml := &mockLLM{responses: []*llm.ChatResponse{{Content: "hi"}}}
	runner := NewRunner(ml, &mockToolExec{}, &mockToolProv{}, DefaultSubAgentConfig(), zap.NewNop())

	result := runner.Run(ctx, RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "sys"}},
	}, nil)

	if !result.Done {
		t.Fatal("expected done=true on cancellation")
	}
	if result.Content != "Request cancelled" {
		t.Fatalf("unexpected content: %q", result.Content)
	}
}

func TestRunner_FailureTrackerInjectsStepBack(t *testing.T) {
	// Tool fails 3 times in a row
	responses := make([]*llm.ChatResponse, 5)
	for i := range 4 {
		responses[i] = &llm.ChatResponse{
			Content:   "trying",
			ToolCalls: []models.ToolCall{{ID: "tc", Name: "run_tests", Args: json.RawMessage(`{}`)}},
		}
	}
	responses[4] = &llm.ChatResponse{Content: "giving up"}

	ml := &mockLLM{responses: responses}
	te := &mockToolExec{results: map[string]*models.ToolResult{
		"run_tests": {ToolCallID: "tc", Content: "FAIL", IsError: true},
	}}
	cfg := Config{MaxSteps: 8, MaxContextTokens: 128000, LLMRetries: 1}
	runner := NewRunner(ml, te, &mockToolProv{}, cfg, zap.NewNop())

	var events []models.ReactStreamEvent
	sink := &collectSink{events: &events}

	result := runner.Run(context.Background(), RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "sys"}},
	}, sink)

	if !result.Done {
		t.Fatal("expected done=true")
	}
	// Check that a step-back message was injected into messages
	found := false
	for _, m := range result.Messages {
		if m.Role == models.RoleSystem && len(m.Content) > 0 && contains(m.Content, "FIX LOOP DETECTED") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected step-back message in messages after 3 failures")
	}
}

type collectSink struct {
	events *[]models.ReactStreamEvent
}

func (s *collectSink) Emit(e models.ReactStreamEvent) {
	*s.events = append(*s.events, e)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
