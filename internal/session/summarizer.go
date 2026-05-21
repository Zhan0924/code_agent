// Package session - summarizer.go
// [OPT-13] LLM-powered async context summarization.
// When messages are archived from hot to cold storage, this module
// generates a concise LLM summary instead of naive text truncation.
package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// Summarizer defines the interface for generating conversation summaries.
// This is implemented by LLMSummarizer for production and SimpleSummarizer for fallback.
type Summarizer interface {
	Summarize(ctx context.Context, messages []models.Message, existingSummary string) (string, error)
}

// LLMSummarizer uses a language model to generate high-quality conversation summaries.
type LLMSummarizer struct {
	// chatFn is a function that calls the LLM. We use a function type instead of
	// importing llm.Client to avoid circular dependencies.
	chatFn func(ctx context.Context, systemPrompt, userPrompt string) (string, error)
	logger *zap.Logger
}

// NewLLMSummarizer creates a summarizer backed by an LLM.
func NewLLMSummarizer(
	chatFn func(ctx context.Context, systemPrompt, userPrompt string) (string, error),
	logger *zap.Logger,
) *LLMSummarizer {
	return &LLMSummarizer{chatFn: chatFn, logger: logger}
}

// Summarize generates a concise summary of the conversation messages using an LLM.
func (s *LLMSummarizer) Summarize(ctx context.Context, messages []models.Message, existingSummary string) (string, error) {
	if len(messages) == 0 {
		return existingSummary, nil
	}

	// Build conversation text for the LLM
	var conversation strings.Builder
	if existingSummary != "" {
		fmt.Fprintf(&conversation, "[Previous Summary]: %s\n\n", existingSummary)
	}

	fmt.Fprintf(&conversation, "[New messages to summarize (%d messages)]:\n", len(messages))
	for _, msg := range messages {
		content := msg.Content
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200]) + "..."
		}
		fmt.Fprintf(&conversation, "- [%s]: %s\n", msg.Role, content)
	}

	systemPrompt := `You are a conversation summarizer for a code intelligence agent.
Summarize the conversation concisely, preserving:
1. Key technical decisions and conclusions
2. Code files, functions, and symbols discussed
3. Tool results and their outcomes
4. Unresolved questions or pending actions
Keep the summary under 200 words. Be factual and precise.`

	summary, err := s.chatFn(ctx, systemPrompt, conversation.String())
	if err != nil {
		s.logger.Warn("LLM summarization failed, falling back to simple summary", zap.Error(err))
		return SimpleSummarize(messages, existingSummary), nil
	}

	return summary, nil
}

// SimpleSummarizer is the fallback when LLM is unavailable.
type SimpleSummarizer struct{}

// Summarize generates a basic text summary without LLM.
func (s *SimpleSummarizer) Summarize(_ context.Context, messages []models.Message, existingSummary string) (string, error) {
	return SimpleSummarize(messages, existingSummary), nil
}

// SimpleSummarize generates a non-LLM summary from messages (fallback).
func SimpleSummarize(messages []models.Message, existingSummary string) string {
	var parts []string
	for _, msg := range messages {
		content := msg.Content
		runes := []rune(content)
		if len(runes) > 80 {
			content = string(runes[:80]) + "..."
		}
		parts = append(parts, fmt.Sprintf("[%s]: %s", msg.Role, content))
	}

	newSummary := fmt.Sprintf("Archived %d messages. Key exchanges: %s",
		len(messages), strings.Join(parts, "; "))

	// Trim to prevent unbounded growth
	if len([]rune(newSummary)) > 500 {
		newSummary = string([]rune(newSummary)[:500]) + "..."
	}

	if existingSummary != "" {
		return existingSummary + "\n" + newSummary
	}
	return newSummary
}
