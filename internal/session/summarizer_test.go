package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

func TestSimpleSummarize_Empty(t *testing.T) {
	result := SimpleSummarize(nil, "")
	if result != "Archived 0 messages. Key exchanges: " {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestSimpleSummarize_WithMessages(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleUser, Content: "How do I use context in Go?"},
		{Role: models.RoleAssistant, Content: "Context is used for cancellation and deadlines."},
	}
	result := SimpleSummarize(msgs, "")
	if result == "" {
		t.Fatal("expected non-empty summary")
	}
	if len(result) > 600 {
		t.Errorf("summary too long: %d chars", len(result))
	}
}

func TestSimpleSummarize_WithExisting(t *testing.T) {
	result := SimpleSummarize(
		[]models.Message{{Role: models.RoleUser, Content: "hello"}},
		"Previous context was about Go.",
	)
	if result == "" {
		t.Fatal("expected non-empty summary")
	}
	// Should contain the existing summary
	if len(result) < len("Previous context was about Go.") {
		t.Error("existing summary seems lost")
	}
}

func TestSimpleSummarize_Truncation(t *testing.T) {
	// Create a very long message to test truncation
	longContent := ""
	for i := 0; i < 100; i++ {
		longContent += "This is a very long message that should be truncated. "
	}
	msgs := []models.Message{
		{Role: models.RoleUser, Content: longContent},
	}
	result := SimpleSummarize(msgs, "")
	// Should be truncated to ~500 chars
	if len([]rune(result)) > 510 {
		t.Errorf("expected truncation, got %d runes", len([]rune(result)))
	}
}

func TestSimpleSummarizer_Interface(t *testing.T) {
	var s Summarizer = &SimpleSummarizer{}
	result, err := s.Summarize(context.Background(),
		[]models.Message{{Role: models.RoleUser, Content: "test"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}
}

func TestLLMSummarizer_Success(t *testing.T) {
	mockFn := func(_ context.Context, system, user string) (string, error) {
		return "Discussed Go context patterns and error handling.", nil
	}
	s := NewLLMSummarizer(mockFn, zap.NewNop())

	msgs := []models.Message{
		{Role: models.RoleUser, Content: "How does context work?"},
		{Role: models.RoleAssistant, Content: "Context provides cancellation..."},
	}
	result, err := s.Summarize(context.Background(), msgs, "")
	if err != nil {
		t.Fatal(err)
	}
	if result != "Discussed Go context patterns and error handling." {
		t.Errorf("unexpected: %q", result)
	}
}

func TestLLMSummarizer_FallbackOnError(t *testing.T) {
	mockFn := func(_ context.Context, system, user string) (string, error) {
		return "", fmt.Errorf("LLM unavailable")
	}
	s := NewLLMSummarizer(mockFn, zap.NewNop())

	msgs := []models.Message{
		{Role: models.RoleUser, Content: "test message"},
	}
	result, err := s.Summarize(context.Background(), msgs, "")
	if err != nil {
		t.Fatal(err)
	}
	// Should fall back to SimpleSummarize
	if result == "" {
		t.Fatal("expected fallback summary")
	}
}

func TestLLMSummarizer_EmptyMessages(t *testing.T) {
	mockFn := func(_ context.Context, system, user string) (string, error) {
		t.Fatal("should not be called for empty messages")
		return "", nil
	}
	s := NewLLMSummarizer(mockFn, zap.NewNop())

	result, err := s.Summarize(context.Background(), nil, "existing")
	if err != nil {
		t.Fatal(err)
	}
	if result != "existing" {
		t.Errorf("expected 'existing', got %q", result)
	}
}
