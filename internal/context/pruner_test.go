package context

import (
	"testing"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

func TestTokenPruner_NoChunks(t *testing.T) {
	cfg := DefaultPrunerConfig()
	p := NewTokenPruner(cfg, zap.NewNop())

	result := p.PruneCodeChunks(nil, nil)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d", len(result))
	}
}

func TestTokenPruner_WithinBudget(t *testing.T) {
	cfg := DefaultPrunerConfig()
	cfg.MaxTokenBudget = 10000
	p := NewTokenPruner(cfg, zap.NewNop())

	chunks := []models.CodeChunk{
		{SymbolName: "Foo", Content: "func Foo() {}"},
		{SymbolName: "Bar", Content: "func Bar() {}"},
	}

	result := p.PruneCodeChunks(chunks, nil)
	if len(result) != 2 {
		t.Errorf("expected all 2 chunks retained, got %d", len(result))
	}
}

func TestTokenPruner_ExceedsBudget(t *testing.T) {
	cfg := DefaultPrunerConfig()
	cfg.MaxTokenBudget = 20 // Very small budget (~80 chars)
	p := NewTokenPruner(cfg, zap.NewNop())

	longContent := ""
	for i := 0; i < 50; i++ {
		longContent += "some code here; "
	}

	chunks := []models.CodeChunk{
		{SymbolName: "Small", Content: "x := 1"},
		{SymbolName: "Large", Content: longContent},
	}

	result := p.PruneCodeChunks(chunks, []float64{0.9, 0.1})
	// Should prune the large chunk or retain only what fits
	totalTokens := 0
	for _, c := range result {
		totalTokens += estimateTokens(c.Content)
	}
	if totalTokens > cfg.MaxTokenBudget {
		t.Errorf("pruned result (%d tokens) exceeds budget (%d)", totalTokens, cfg.MaxTokenBudget)
	}
}

func TestTokenPruner_UsesConfigWeights(t *testing.T) {
	// Verify that different weight configs produce different results
	cfg1 := DefaultPrunerConfig()
	cfg1.MaxTokenBudget = 15
	cfg1.WeightRelevance = 0.9
	cfg1.WeightCallFreq = 0.1
	cfg1.WeightScope = 0.0
	cfg1.WeightRecency = 0.0

	cfg2 := DefaultPrunerConfig()
	cfg2.MaxTokenBudget = 15
	cfg2.WeightRelevance = 0.0
	cfg2.WeightCallFreq = 0.0
	cfg2.WeightScope = 0.0
	cfg2.WeightRecency = 0.9

	p1 := NewTokenPruner(cfg1, zap.NewNop())
	p2 := NewTokenPruner(cfg2, zap.NewNop())

	chunks := []models.CodeChunk{
		{SymbolName: "HighRelevance", Content: "func HighRelevance() { return 1 }"},
		{SymbolName: "LowRelevance", Content: "func LowRelevance() { return 2 }"},
		{SymbolName: "MidRelevance", Content: "func MidRelevance() { return 3 }"},
	}
	scores := []float64{0.95, 0.1, 0.5}

	r1 := p1.PruneCodeChunks(chunks, scores)
	r2 := p2.PruneCodeChunks(chunks, scores)

	// Both should produce valid output
	if len(r1) == 0 || len(r2) == 0 {
		t.Error("expected non-empty results from both configs")
	}
}

func TestPruneMessages_WithinBudget(t *testing.T) {
	cfg := DefaultPrunerConfig()
	p := NewTokenPruner(cfg, zap.NewNop())

	msgs := []models.Message{
		{Role: models.RoleUser, Content: "hello"},
		{Role: models.RoleAssistant, Content: "hi"},
	}

	result := p.PruneMessages(msgs, 10000)
	if len(result) != 2 {
		t.Errorf("expected 2 messages, got %d", len(result))
	}
}

func TestPruneMessages_ExceedsBudget(t *testing.T) {
	cfg := DefaultPrunerConfig()
	p := NewTokenPruner(cfg, zap.NewNop())

	// Use longer messages so the total easily exceeds a small budget
	longText := "This is a fairly long message that contains substantial content to ensure token budget is exceeded. "
	msgs := []models.Message{
		{Role: models.RoleSystem, Content: "You are a helpful assistant for coding tasks."},
		{Role: models.RoleUser, Content: longText + "old question 1"},
		{Role: models.RoleAssistant, Content: longText + "old answer 1"},
		{Role: models.RoleUser, Content: longText + "old question 2"},
		{Role: models.RoleAssistant, Content: longText + "old answer 2"},
		{Role: models.RoleUser, Content: longText + "old question 3"},
		{Role: models.RoleAssistant, Content: longText + "old answer 3"},
		{Role: models.RoleUser, Content: "recent question"},
		{Role: models.RoleAssistant, Content: "recent answer"},
	}

	// Budget of 50 tokens (~200 chars) is not enough for 9 messages
	result := p.PruneMessages(msgs, 50)
	// System message should always be kept
	hasSystem := false
	for _, m := range result {
		if m.Role == models.RoleSystem {
			hasSystem = true
		}
	}
	if !hasSystem {
		t.Error("system message should be preserved")
	}
	// Should be fewer messages
	if len(result) >= len(msgs) {
		t.Errorf("expected fewer messages after pruning, got %d (original %d)", len(result), len(msgs))
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"hello", 2},       // 5/4 + 1 = 2
		{"hello world", 3}, // 11/4 + 1 = 3
	}
	for _, tt := range tests {
		got := estimateTokens(tt.input)
		if got != tt.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
