package memory

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// LLMCaller abstracts the LLM client for memory extraction.
type LLMCaller interface {
	ChatCompletion(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
}

// MemoryStorer abstracts the memory store for the extractor.
type MemoryStorer interface {
	Store(ctx context.Context, m *Memory) error
	Retrieve(ctx context.Context, userID, projectID, query string, limit int) ([]Memory, error)
}

// Extractor analyzes interactions and extracts structured memories using LLM.
type Extractor struct {
	store  MemoryStorer
	llm    LLMCaller
	logger *zap.Logger
}

// NewExtractor creates a memory extractor.
// If llm is nil, falls back to heuristic extraction.
func NewExtractor(store MemoryStorer, llmCaller LLMCaller, logger *zap.Logger) *Extractor {
	return &Extractor{
		store:  store,
		llm:    llmCaller,
		logger: logger.With(zap.String("component", "memory.extractor")),
	}
}

// ExtractedMemory is the structured output from LLM extraction.
type ExtractedMemory struct {
	Type       string  `json:"type"`
	Content    string  `json:"content"`
	Importance float64 `json:"importance"`
}

// ExtractFromInteraction analyzes a completed interaction and stores relevant memories.
func (e *Extractor) ExtractFromInteraction(ctx context.Context, userID, projectID, userMsg, assistantMsg string) {
	var candidates []ExtractedMemory

	if e.llm != nil {
		candidates = e.extractWithLLM(ctx, userMsg, assistantMsg)
	} else {
		candidates = e.extractWithHeuristics(userMsg, assistantMsg)
	}

	stored := 0
	for _, c := range candidates {
		if c.Content == "" || c.Importance < 0.3 {
			continue
		}

		memType := parseMemoryType(c.Type)

		// Deduplication: check existing memories for similar content
		if e.isDuplicate(ctx, userID, projectID, c.Content) {
			e.logger.Debug("skipping duplicate memory", zap.String("content", truncate(c.Content, 80)))
			continue
		}

		m := Memory{
			ID:             uuid.New().String(),
			UserID:         userID,
			ProjectID:      projectID,
			Type:           memType,
			Content:        c.Content,
			Score:          c.Importance,
			CreatedAt:      time.Now(),
			LastAccessedAt: time.Now(),
		}

		if err := e.store.Store(ctx, &m); err != nil {
			e.logger.Debug("failed to store memory", zap.Error(err))
		} else {
			stored++
		}
	}

	if stored > 0 {
		e.logger.Info("memories extracted",
			zap.String("user_id", userID),
			zap.Int("stored", stored),
			zap.Int("candidates", len(candidates)))
	}
}

const extractionPrompt = `Analyze this interaction and extract important memories worth remembering for future conversations. Focus on:
- User preferences (coding style, tools, language, workflow preferences)
- Technical decisions (architecture choices, library selections, design patterns)
- Project knowledge (file structure insights, domain-specific facts, constraints)
- Behavioral patterns (common requests, recurring problems, workflow habits)

Rules:
- Only extract genuinely useful, specific information (not generic facts)
- Each memory should be a concise, self-contained statement (1-2 sentences max)
- Score importance 0.0-1.0: 1.0 = critical preference/decision, 0.5 = useful context, 0.3 = minor detail
- If nothing worth remembering, return empty array

Respond with ONLY a JSON array:
[{"type": "preference|decision|knowledge|pattern", "content": "...", "importance": 0.0-1.0}]

User message:
%s

Assistant response:
%s`

func (e *Extractor) extractWithLLM(ctx context.Context, userMsg, assistantMsg string) []ExtractedMemory {
	// Truncate inputs to avoid excessive token usage
	userTrunc := truncate(userMsg, 2000)
	assistTrunc := truncate(assistantMsg, 3000)

	prompt := strings.Replace(
		strings.Replace(extractionPrompt, "%s", userTrunc, 1),
		"%s", assistTrunc, 1)

	resp, err := e.llm.ChatCompletion(ctx, &llm.ChatRequest{
		Messages: []models.Message{
			{Role: models.RoleUser, Content: prompt},
		},
		Temperature: 0.1,
		MaxTokens:   1024,
	})
	if err != nil {
		e.logger.Debug("LLM extraction failed, falling back to heuristics", zap.Error(err))
		return e.extractWithHeuristics(userMsg, assistantMsg)
	}

	return parseLLMResponse(resp.Content)
}

func parseLLMResponse(content string) []ExtractedMemory {
	content = strings.TrimSpace(content)
	// Strip markdown code fences if present
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		if len(lines) > 2 {
			content = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var memories []ExtractedMemory
	if err := json.Unmarshal([]byte(content), &memories); err != nil {
		return nil
	}

	// Validate and clamp
	valid := memories[:0]
	for _, m := range memories {
		if m.Content == "" {
			continue
		}
		if m.Importance < 0 {
			m.Importance = 0
		}
		if m.Importance > 1 {
			m.Importance = 1
		}
		valid = append(valid, m)
	}
	return valid
}

// extractWithHeuristics is the fallback when no LLM is available.
// Improved over the original: extracts concise insights, not raw messages.
func (e *Extractor) extractWithHeuristics(userMsg, assistantMsg string) []ExtractedMemory {
	var memories []ExtractedMemory

	if insight, importance := extractPreferenceInsight(userMsg); insight != "" {
		memories = append(memories, ExtractedMemory{
			Type:       string(MemoryPreference),
			Content:    insight,
			Importance: importance,
		})
	}

	if insight, importance := extractDecisionInsight(assistantMsg); insight != "" {
		memories = append(memories, ExtractedMemory{
			Type:       string(MemoryDecision),
			Content:    insight,
			Importance: importance,
		})
	}

	return memories
}

// isDuplicate checks if a similar memory already exists.
func (e *Extractor) isDuplicate(ctx context.Context, userID, projectID, content string) bool {
	existing, err := e.store.Retrieve(ctx, userID, projectID, content, 5)
	if err != nil || len(existing) == 0 {
		return false
	}

	contentLower := strings.ToLower(content)
	for _, m := range existing {
		if textSimilarity(contentLower, strings.ToLower(m.Content)) > 0.7 {
			return true
		}
	}
	return false
}

// textSimilarity computes a simple Jaccard similarity on word sets.
func textSimilarity(a, b string) float64 {
	wordsA := wordSet(a)
	wordsB := wordSet(b)

	if len(wordsA) == 0 || len(wordsB) == 0 {
		return 0
	}

	intersection := 0
	for w := range wordsA {
		if wordsB[w] {
			intersection++
		}
	}

	union := len(wordsA) + len(wordsB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func wordSet(s string) map[string]bool {
	words := strings.Fields(s)
	set := make(map[string]bool, len(words))
	for _, w := range words {
		if len(w) > 2 {
			set[w] = true
		}
	}
	return set
}

func parseMemoryType(t string) MemoryType {
	switch MemoryType(t) {
	case MemoryPreference, MemoryDecision, MemoryKnowledge, MemoryPattern:
		return MemoryType(t)
	default:
		return MemoryKnowledge
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// Improved heuristic extractors that produce concise insights.

var prefPhrases = []struct {
	phrase     string
	importance float64
}{
	{"from now on", 0.9}, {"please always", 0.85}, {"please never", 0.85},
	{"i prefer", 0.8}, {"i always", 0.7}, {"i never", 0.7},
	{"i like", 0.6}, {"i don't like", 0.6},
	{"我偏好", 0.8}, {"我喜欢", 0.6}, {"我总是", 0.7}, {"我从不", 0.7}, {"以后", 0.8},
}

func extractPreferenceInsight(msg string) (string, float64) {
	lower := strings.ToLower(msg)
	for _, p := range prefPhrases {
		if strings.Contains(lower, p.phrase) {
			// Extract the sentence containing the phrase
			insight := extractSentence(msg, p.phrase)
			if insight == "" {
				insight = truncate(msg, 200)
			}
			return insight, p.importance
		}
	}
	return "", 0
}

var decisionPhrases = []struct {
	phrase     string
	importance float64
}{
	{"architecture decision", 0.9}, {"i've decided", 0.85},
	{"let's go with", 0.8}, {"we'll use", 0.75}, {"the approach is", 0.7},
	{"架构决策", 0.9}, {"决定使用", 0.85}, {"方案是", 0.8},
}

func extractDecisionInsight(msg string) (string, float64) {
	lower := strings.ToLower(msg)
	for _, p := range decisionPhrases {
		if strings.Contains(lower, p.phrase) {
			insight := extractSentence(msg, p.phrase)
			if insight == "" {
				insight = truncate(msg, 300)
			}
			return insight, p.importance
		}
	}
	return "", 0
}

// extractSentence finds the sentence containing the phrase.
func extractSentence(text, phrase string) string {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, phrase)
	if idx < 0 {
		return ""
	}

	// Find sentence boundaries
	start := idx
	for start > 0 && text[start-1] != '.' && text[start-1] != '\n' && text[start-1] != '!' && text[start-1] != '?' {
		start--
	}

	end := idx + len(phrase)
	for end < len(text) && text[end] != '.' && text[end] != '\n' && text[end] != '!' && text[end] != '?' {
		end++
	}
	if end < len(text) {
		end++
	}

	sentence := strings.TrimSpace(text[start:end])
	if len(sentence) > 300 {
		return sentence[:300]
	}
	return sentence
}
