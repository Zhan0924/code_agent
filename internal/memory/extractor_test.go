package memory

import (
	"context"
	"testing"

	"github.com/agent/code_agent/internal/llm"
	"go.uber.org/zap"
)

// mockLLM returns pre-configured responses.
type mockLLM struct {
	response string
	err      error
}

func (m *mockLLM) ChatCompletion(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &llm.ChatResponse{Content: m.response}, nil
}

// mockStore tracks stored memories.
type mockStore struct {
	stored   []*Memory
	existing []Memory
}

func (m *mockStore) Store(_ context.Context, mem *Memory) error {
	m.stored = append(m.stored, mem)
	return nil
}

func (m *mockStore) Retrieve(_ context.Context, _, _, _ string, _ int) ([]Memory, error) {
	return m.existing, nil
}

func TestExtractor_LLMExtraction(t *testing.T) {
	llmResp := `[
		{"type": "preference", "content": "User prefers using Go for backend services", "importance": 0.8},
		{"type": "decision", "content": "Decided to use PostgreSQL for persistence", "importance": 0.9}
	]`

	ml := &mockLLM{response: llmResp}
	ms := &mockStore{}
	ext := NewExtractor(ms, ml, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1",
		"I prefer Go for backend", "Let's use PostgreSQL")

	if len(ms.stored) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(ms.stored))
	}

	if ms.stored[0].Type != MemoryPreference {
		t.Errorf("expected preference, got %s", ms.stored[0].Type)
	}
	if ms.stored[0].Score != 0.8 {
		t.Errorf("expected score 0.8, got %f", ms.stored[0].Score)
	}

	if ms.stored[1].Type != MemoryDecision {
		t.Errorf("expected decision, got %s", ms.stored[1].Type)
	}
	if ms.stored[1].Score != 0.9 {
		t.Errorf("expected score 0.9, got %f", ms.stored[1].Score)
	}
}

func TestExtractor_LLMExtraction_WithCodeFence(t *testing.T) {
	llmResp := "```json\n" + `[{"type": "knowledge", "content": "API uses REST", "importance": 0.7}]` + "\n```"

	ml := &mockLLM{response: llmResp}
	ms := &mockStore{}
	ext := NewExtractor(ms, ml, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1", "test", "test")

	if len(ms.stored) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(ms.stored))
	}
	if ms.stored[0].Content != "API uses REST" {
		t.Errorf("unexpected content: %s", ms.stored[0].Content)
	}
}

func TestExtractor_LLMExtraction_FiltersLowImportance(t *testing.T) {
	llmResp := `[
		{"type": "knowledge", "content": "Important fact", "importance": 0.8},
		{"type": "knowledge", "content": "Trivial detail", "importance": 0.2}
	]`

	ml := &mockLLM{response: llmResp}
	ms := &mockStore{}
	ext := NewExtractor(ms, ml, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1", "test", "test")

	if len(ms.stored) != 1 {
		t.Fatalf("expected 1 memory (filtered low importance), got %d", len(ms.stored))
	}
	if ms.stored[0].Content != "Important fact" {
		t.Errorf("wrong memory stored: %s", ms.stored[0].Content)
	}
}

func TestExtractor_Deduplication(t *testing.T) {
	llmResp := `[{"type": "preference", "content": "User prefers using tabs for indentation", "importance": 0.8}]`

	ml := &mockLLM{response: llmResp}
	ms := &mockStore{
		existing: []Memory{
			{Content: "User prefers tabs for indentation in code"},
		},
	}
	ext := NewExtractor(ms, ml, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1", "I prefer tabs", "ok")

	if len(ms.stored) != 0 {
		t.Fatalf("expected 0 memories (duplicate), got %d", len(ms.stored))
	}
}

func TestExtractor_HeuristicFallback(t *testing.T) {
	ms := &mockStore{}
	ext := NewExtractor(ms, nil, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1",
		"From now on, please use snake_case for variables", "Got it")

	if len(ms.stored) != 1 {
		t.Fatalf("expected 1 memory from heuristic, got %d", len(ms.stored))
	}
	if ms.stored[0].Type != MemoryPreference {
		t.Errorf("expected preference, got %s", ms.stored[0].Type)
	}
	if ms.stored[0].Score < 0.8 {
		t.Errorf("expected high importance for 'from now on', got %f", ms.stored[0].Score)
	}
}

func TestExtractor_HeuristicDecision(t *testing.T) {
	ms := &mockStore{}
	ext := NewExtractor(ms, nil, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1",
		"What should we use?", "I've decided to use Redis for caching")

	if len(ms.stored) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(ms.stored))
	}
	if ms.stored[0].Type != MemoryDecision {
		t.Errorf("expected decision, got %s", ms.stored[0].Type)
	}
}

func TestTextSimilarity(t *testing.T) {
	tests := []struct {
		a, b     string
		minScore float64
	}{
		{"user prefers tabs", "user prefers tabs", 0.99},
		{"user prefers tabs for indentation", "user prefers tabs in code", 0.5},
		{"completely different text", "nothing in common here", 0.0},
	}

	for _, tt := range tests {
		score := textSimilarity(tt.a, tt.b)
		if score < tt.minScore {
			t.Errorf("textSimilarity(%q, %q) = %f, want >= %f", tt.a, tt.b, score, tt.minScore)
		}
	}
}

func TestParseLLMResponse_InvalidJSON(t *testing.T) {
	result := parseLLMResponse("not json at all")
	if result != nil {
		t.Errorf("expected nil for invalid JSON, got %v", result)
	}
}

func TestParseLLMResponse_ClampsImportance(t *testing.T) {
	llmResp := `[
		{"type": "knowledge", "content": "test", "importance": -0.5},
		{"type": "knowledge", "content": "test2", "importance": 1.5}
	]`

	result := parseLLMResponse(llmResp)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if result[0].Importance != 0 {
		t.Errorf("expected clamped to 0, got %f", result[0].Importance)
	}
	if result[1].Importance != 1 {
		t.Errorf("expected clamped to 1, got %f", result[1].Importance)
	}
}

func TestExtractSentence(t *testing.T) {
	text := "This is the first sentence. I prefer using Go for backend services. This is another sentence."
	result := extractSentence(text, "i prefer")

	if result == "" {
		t.Fatal("expected non-empty result")
	}
	if !contains(result, "prefer") {
		t.Errorf("result should contain 'prefer': %s", result)
	}
	// Should extract just the sentence, not the whole text
	if len(result) > 100 {
		t.Errorf("sentence too long: %d chars", len(result))
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexSubstr(s, substr) >= 0)
}

func indexSubstr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
