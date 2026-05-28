package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

const (
	compactionInterval   = 5
	compactionKeepRecent = 3
	compactionMaxChars   = 2000
	compactionHeadChars  = 400
	compactionTailChars  = 400
)

// compactEarlyMessages compresses old tool_result messages in-place to free context budget.
// mode: "summarize" uses LLM to produce a concise summary; "truncate" (default) uses head+tail truncation.
// When mode is "summarize", llmClient and logger must be non-nil.
func compactEarlyMessages(messages []models.Message, currentStep int, opts ...compactionOption) {
	if currentStep < compactionInterval {
		return
	}
	if currentStep%compactionInterval != 0 {
		return
	}

	var cfg compactionConfig
	for _, o := range opts {
		o(&cfg)
	}

	toolResultCount := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == models.RoleTool {
			toolResultCount++
		}
	}

	if toolResultCount <= compactionKeepRecent {
		return
	}

	compactUpTo := toolResultCount - compactionKeepRecent
	toolIdx := 0

	for i := range messages {
		if messages[i].Role != models.RoleTool {
			continue
		}
		toolIdx++
		if toolIdx > compactUpTo {
			break
		}

		content := messages[i].Content
		runes := []rune(content)
		if len(runes) <= compactionMaxChars {
			continue
		}

		if strings.Contains(content, "[compacted:") || strings.Contains(content, "[summarized]") {
			continue
		}

		if cfg.mode == "summarize" && cfg.llmClient != nil {
			if summary, err := summarizeToolResult(cfg.llmClient, content, cfg.logger); err == nil {
				messages[i].Content = fmt.Sprintf("[summarized] %s", summary)
				continue
			}
		}

		// Fallback: head+tail truncation
		head := string(runes[:compactionHeadChars])
		tail := string(runes[len(runes)-compactionTailChars:])
		omitted := len(runes) - compactionHeadChars - compactionTailChars
		messages[i].Content = fmt.Sprintf("%s\n\n[compacted: %d chars omitted]\n\n%s", head, omitted, tail)
	}
}

type compactionConfig struct {
	mode      string
	llmClient *llm.Client
	logger    *zap.Logger
}

type compactionOption func(*compactionConfig)

func withSummarizeMode(client *llm.Client, logger *zap.Logger) compactionOption {
	return func(c *compactionConfig) {
		c.mode = "summarize"
		c.llmClient = client
		c.logger = logger
	}
}

// summarizeToolResult calls LLM to produce a concise summary of a tool result.
// Times out after 5s; returns error on failure so caller can fallback to truncation.
func summarizeToolResult(client *llm.Client, content string, logger *zap.Logger) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.ChatCompletion(ctx, &llm.ChatRequest{
		Messages: []models.Message{
			{
				Role:    models.RoleSystem,
				Content: "Summarize the following tool output in 2-3 concise sentences. Preserve key facts, numbers, file paths, and error messages. Omit verbose formatting and repetition.",
			},
			{
				Role:    models.RoleUser,
				Content: content,
			},
		},
		Temperature: 0.2,
		MaxTokens:   500,
	})
	if err != nil {
		if logger != nil {
			logger.Warn("summarize tool result failed, falling back to truncation", zap.Error(err))
		}
		return "", err
	}

	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return "", fmt.Errorf("empty summary from LLM")
	}
	return summary, nil
}
