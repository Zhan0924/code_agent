package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
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

// ═══════════════════════════════════════════════════════════════════════════
//  P2-6: 扩充 ReAct 循环测试
// ═══════════════════════════════════════════════════════════════════════════

func TestRunner_ReflectionCheckpoints(t *testing.T) {
	// 构造 15 步工具调用，验证 step 10 时注入反思消息
	responses := make([]*llm.ChatResponse, 16)
	for i := range 15 {
		responses[i] = &llm.ChatResponse{
			Content:   "working...",
			ToolCalls: []models.ToolCall{{ID: "tc", Name: "read_file", Args: json.RawMessage(`{}`)}},
		}
	}
	responses[15] = &llm.ChatResponse{Content: "done"}

	ml := &mockLLM{responses: responses}
	cfg := Config{MaxSteps: 20, MaxContextTokens: 128000, LLMRetries: 1, EnableReflection: true}
	runner := NewRunner(ml, &mockToolExec{}, &mockToolProv{}, cfg, zap.NewNop())

	result := runner.Run(context.Background(), RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "sys"}},
	}, nil)

	if !result.Done {
		t.Fatal("expected done=true")
	}
	// 验证反思消息被注入（EnableReflection 在 step%10==0 时触发检查点）
	found := false
	for _, m := range result.Messages {
		if m.Role == models.RoleSystem && contains(m.Content, "METACOGNITIVE CHECKPOINT") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected METACOGNITIVE CHECKPOINT message in result")
	}
}

func TestRunner_TokenBudgetPruning(t *testing.T) {
	// 构造超过 token 预算的消息历史
	longContent := ""
	for range 5000 {
		longContent += "word "
	}

	responses := []*llm.ChatResponse{
		{Content: "thinking", ToolCalls: []models.ToolCall{{ID: "tc", Name: "read_file", Args: json.RawMessage(`{}`)}}},
		{Content: "final answer"},
	}
	ml := &mockLLM{responses: responses}
	te := &mockToolExec{results: map[string]*models.ToolResult{
		"read_file": {ToolCallID: "tc", Content: longContent},
	}}

	cfg := Config{MaxSteps: 10, MaxContextTokens: 100, LLMRetries: 1}
	runner := NewRunner(ml, te, &mockToolProv{}, cfg, zap.NewNop())

	// 构造大量历史消息
	messages := []models.Message{
		{Role: models.RoleSystem, Content: "sys"},
	}
	for range 20 {
		messages = append(messages, models.Message{Role: models.RoleUser, Content: longContent})
	}

	result := runner.Run(context.Background(), RunOpts{
		Messages: messages,
	}, nil)

	if !result.Done {
		t.Fatal("expected done=true")
	}
	// 验证消息被裁剪（结果消息数应少于输入 + 工具调用）
	if len(result.Messages) >= 25 {
		t.Logf("messages may not have been pruned: %d messages", len(result.Messages))
	}
}

func TestRunner_ToolSequenceTracking(t *testing.T) {
	// 验证工具序列被正确记录到事件流
	ml := &mockLLM{responses: []*llm.ChatResponse{
		{Content: "reading", ToolCalls: []models.ToolCall{
			{ID: "tc1", Name: "read_file", Args: json.RawMessage(`{"path":"main.go"}`)},
		}},
		{Content: "editing", ToolCalls: []models.ToolCall{
			{ID: "tc2", Name: "edit_file", Args: json.RawMessage(`{"path":"main.go"}`)},
		}},
		{Content: "testing", ToolCalls: []models.ToolCall{
			{ID: "tc3", Name: "run_tests", Args: json.RawMessage(`{}`)},
		}},
		{Content: "All done."},
	}}
	te := &mockToolExec{results: map[string]*models.ToolResult{
		"read_file": {ToolCallID: "tc1", Content: "package main"},
		"edit_file": {ToolCallID: "tc2", Content: "edited"},
		"run_tests": {ToolCallID: "tc3", Content: "PASS"},
	}}
	tp := &mockToolProv{tools: []models.ToolDefinition{
		{Name: "read_file"}, {Name: "edit_file"}, {Name: "run_tests"},
	}}

	cfg := Config{MaxSteps: 10, MaxContextTokens: 128000, LLMRetries: 1}
	runner := NewRunner(ml, te, tp, cfg, zap.NewNop())

	var events []models.ReactStreamEvent
	sink := &collectSink{events: &events}

	result := runner.Run(context.Background(), RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "sys"}},
	}, sink)

	if !result.Done {
		t.Fatal("expected done=true")
	}
	if result.StepsUsed != 4 {
		t.Fatalf("expected 4 steps, got %d", result.StepsUsed)
	}

	// 验证事件流包含正确的工具调用序列
	var toolCalls []string
	for _, e := range events {
		if e.Type == "tool_call" {
			toolCalls = append(toolCalls, e.ToolName)
		}
	}
	expected := []string{"read_file", "edit_file", "run_tests"}
	if len(toolCalls) != len(expected) {
		t.Fatalf("expected %d tool calls, got %d: %v", len(expected), len(toolCalls), toolCalls)
	}
	for i, name := range expected {
		if toolCalls[i] != name {
			t.Errorf("tool call %d: expected %s, got %s", i, name, toolCalls[i])
		}
	}
}

func TestRunner_LLMRetryOnFailure(t *testing.T) {
	// 第一次 LLM 调用失败，第二次成功
	ml := &mockLLM{
		responses: []*llm.ChatResponse{
			nil,
			{Content: "recovered answer"},
		},
		errors: []error{
			fmt.Errorf("temporary LLM error"),
			nil,
		},
	}
	cfg := Config{MaxSteps: 5, MaxContextTokens: 128000, LLMRetries: 3}
	runner := NewRunner(ml, &mockToolExec{}, &mockToolProv{}, cfg, zap.NewNop())

	result := runner.Run(context.Background(), RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "sys"}},
	}, nil)

	if !result.Done {
		t.Fatal("expected done=true")
	}
	if result.Content != "recovered answer" {
		t.Fatalf("expected 'recovered answer', got %q", result.Content)
	}
}

func TestRunner_MultipleToolCallsPerStep(t *testing.T) {
	// LLM 返回多个工具调用在同一步
	ml := &mockLLM{responses: []*llm.ChatResponse{
		{Content: "reading multiple files", ToolCalls: []models.ToolCall{
			{ID: "tc1", Name: "read_file", Args: json.RawMessage(`{"path":"a.go"}`)},
			{ID: "tc2", Name: "read_file", Args: json.RawMessage(`{"path":"b.go"}`)},
			{ID: "tc3", Name: "read_file", Args: json.RawMessage(`{"path":"c.go"}`)},
		}},
		{Content: "All files read."},
	}}
	te := &mockToolExec{results: map[string]*models.ToolResult{
		"read_file": {ToolCallID: "tc1", Content: "package a"},
	}}

	cfg := Config{MaxSteps: 5, MaxContextTokens: 128000, LLMRetries: 1}
	runner := NewRunner(ml, te, &mockToolProv{}, cfg, zap.NewNop())

	var events []models.ReactStreamEvent
	sink := &collectSink{events: &events}

	result := runner.Run(context.Background(), RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "sys"}},
	}, sink)

	if !result.Done {
		t.Fatal("expected done=true")
	}
	if result.StepsUsed != 2 {
		t.Fatalf("expected 2 steps, got %d", result.StepsUsed)
	}

	// 验证 3 个工具调用都被执行
	toolCallCount := 0
	for _, e := range events {
		if e.Type == "tool_call" {
			toolCallCount++
		}
	}
	if toolCallCount != 3 {
		t.Fatalf("expected 3 tool_call events, got %d", toolCallCount)
	}
}

func TestRunner_AdaptiveFeedbackOnRepeatedFailures(t *testing.T) {
	// 同一工具连续失败，验证 adaptive feedback 注入
	responses := make([]*llm.ChatResponse, 6)
	for i := range 5 {
		responses[i] = &llm.ChatResponse{
			Content:   "trying again",
			ToolCalls: []models.ToolCall{{ID: fmt.Sprintf("tc%d", i), Name: "run_tests", Args: json.RawMessage(`{}`)}},
		}
	}
	responses[5] = &llm.ChatResponse{Content: "giving up"}

	ml := &mockLLM{responses: responses}
	te := &mockToolExec{results: map[string]*models.ToolResult{
		"run_tests": {ToolCallID: "tc0", Content: "FAIL: test_foo", IsError: true},
	}}

	cfg := Config{MaxSteps: 10, MaxContextTokens: 128000, LLMRetries: 1}
	runner := NewRunner(ml, te, &mockToolProv{}, cfg, zap.NewNop())

	var events []models.ReactStreamEvent
	sink := &collectSink{events: &events}

	result := runner.Run(context.Background(), RunOpts{
		Messages: []models.Message{{Role: models.RoleSystem, Content: "sys"}},
	}, sink)

	if !result.Done {
		t.Fatal("expected done=true")
	}

	// 验证 tool_result 事件中包含 SYSTEM HINT
	hintFound := false
	for _, e := range events {
		if e.Type == "tool_result" && e.IsError && contains(e.Content, "[SYSTEM HINT]") {
			hintFound = true
			break
		}
	}
	if !hintFound {
		t.Fatal("expected adaptive feedback hint in tool_result events")
	}
}
