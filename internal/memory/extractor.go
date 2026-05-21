package memory

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Extractor analyzes successful interactions and extracts new memories.
type Extractor struct {
	store  *HybridStore
	logger *zap.Logger
}

// NewExtractor creates a memory extractor.
func NewExtractor(store *HybridStore, logger *zap.Logger) *Extractor {
	return &Extractor{
		store:  store,
		logger: logger.With(zap.String("component", "memory.extractor")),
	}
}

// ExtractFromInteraction analyzes a completed interaction and stores relevant memories.
func (e *Extractor) ExtractFromInteraction(ctx context.Context, userID, projectID, userMsg, assistantMsg string) {
	memories := e.analyze(userMsg, assistantMsg)
	for _, m := range memories {
		m.ID = uuid.New().String()
		m.UserID = userID
		m.ProjectID = projectID
		m.Score = 1.0
		m.CreatedAt = time.Now()
		m.LastAccessedAt = time.Now()

		if err := e.store.Store(ctx, &m); err != nil {
			e.logger.Debug("failed to store extracted memory", zap.Error(err))
		}
	}

	if len(memories) > 0 {
		e.logger.Info("memories extracted",
			zap.String("user_id", userID),
			zap.Int("count", len(memories)))
	}
}

func (e *Extractor) analyze(userMsg, assistantMsg string) []Memory {
	var memories []Memory

	if pref := extractPreference(userMsg); pref != "" {
		memories = append(memories, Memory{
			Type:    MemoryPreference,
			Content: pref,
		})
	}

	if decision := extractDecision(assistantMsg); decision != "" {
		memories = append(memories, Memory{
			Type:    MemoryDecision,
			Content: decision,
		})
	}

	return memories
}

func extractPreference(msg string) string {
	lower := strings.ToLower(msg)
	prefPhrases := []string{
		"i prefer", "i always", "i never", "i like", "i don't like",
		"please always", "please never", "from now on",
		"我喜欢", "我偏好", "我总是", "我从不", "以后",
	}
	for _, phrase := range prefPhrases {
		if strings.Contains(lower, phrase) {
			return msg
		}
	}
	return ""
}

func extractDecision(msg string) string {
	lower := strings.ToLower(msg)
	decisionPhrases := []string{
		"i've decided", "let's go with", "we'll use",
		"the approach is", "architecture decision",
		"决定使用", "方案是", "架构决策",
	}
	for _, phrase := range decisionPhrases {
		if strings.Contains(lower, phrase) {
			if len(msg) > 500 {
				return msg[:500]
			}
			return msg
		}
	}
	return ""
}
