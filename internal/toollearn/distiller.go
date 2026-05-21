package toollearn

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// StrategyEntry represents a distilled strategy learned from successful sessions.
type StrategyEntry struct {
	ID          string    `json:"id"`
	TaskPattern string    `json:"task_pattern"`
	ToolChain   []string  `json:"tool_chain"`
	SuccessRate float64   `json:"success_rate"`
	AvgDuration int       `json:"avg_duration_ms"`
	SampleCount int       `json:"sample_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Distiller extracts reusable strategy knowledge from successful tool sequences.
// It identifies recurring patterns of tool usage that lead to success and
// distills them into strategy entries for future sessions.
type Distiller struct {
	mu              sync.RWMutex
	strategies      map[string]*StrategyEntry // taskPattern → strategy
	collector       *Collector
	minSamples      int
	processedOffset int // tracks how many feedback entries have been processed
	logger          *zap.Logger
}

// NewDistiller creates a knowledge distiller backed by a feedback collector.
func NewDistiller(collector *Collector, logger *zap.Logger) *Distiller {
	return &Distiller{
		strategies: make(map[string]*StrategyEntry),
		collector:  collector,
		minSamples: 5,
		logger:     logger.With(zap.String("component", "toollearn.distiller")),
	}
}

// Distill analyzes recent successful sessions and extracts strategy patterns.
func (d *Distiller) Distill() int {
	d.collector.mu.Lock()
	buffer := d.collector.buffer
	startIdx := d.processedOffset
	if startIdx >= len(buffer) {
		d.collector.mu.Unlock()
		return 0
	}
	newFeedback := make([]Feedback, len(buffer)-startIdx)
	copy(newFeedback, buffer[startIdx:])
	d.processedOffset = len(buffer)
	d.collector.mu.Unlock()

	if len(newFeedback) < d.minSamples {
		return 0
	}

	// Group feedback by session
	bySession := make(map[string][]Feedback)
	for _, fb := range newFeedback {
		bySession[fb.SessionID] = append(bySession[fb.SessionID], fb)
	}

	newStrategies := 0
	for _, feedbacks := range bySession {
		if len(feedbacks) < 3 {
			continue
		}

		// Sort by time
		sort.Slice(feedbacks, func(i, j int) bool {
			return feedbacks[i].CreatedAt.Before(feedbacks[j].CreatedAt)
		})

		// Only learn from mostly-successful sessions
		successes := 0
		for _, fb := range feedbacks {
			if fb.Success {
				successes++
			}
		}
		sessionRate := float64(successes) / float64(len(feedbacks))
		if sessionRate < 0.7 {
			continue
		}

		// Extract the tool chain
		chain := extractToolChain(feedbacks)
		pattern := classifyTaskPattern(chain)

		if d.updateStrategy(pattern, chain, feedbacks) {
			newStrategies++
		}
	}

	if newStrategies > 0 {
		d.logger.Info("strategies distilled", zap.Int("new", newStrategies))
	}
	return newStrategies
}

func (d *Distiller) updateStrategy(pattern string, chain []string, feedbacks []Feedback) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	existing, ok := d.strategies[pattern]
	if !ok {
		d.strategies[pattern] = &StrategyEntry{
			ID:          fmt.Sprintf("strat_%d", time.Now().UnixNano()),
			TaskPattern: pattern,
			ToolChain:   chain,
			SuccessRate: computeSuccessRate(feedbacks),
			AvgDuration: computeAvgDuration(feedbacks),
			SampleCount: 1,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		return true
	}

	// Merge: update running averages
	existing.SampleCount++
	newRate := computeSuccessRate(feedbacks)
	existing.SuccessRate = existing.SuccessRate*0.8 + newRate*0.2
	existing.AvgDuration = (existing.AvgDuration*(existing.SampleCount-1) + computeAvgDuration(feedbacks)) / existing.SampleCount
	existing.UpdatedAt = time.Now()

	// Update chain if new one is shorter (more efficient)
	if len(chain) < len(existing.ToolChain) && newRate >= 0.8 {
		existing.ToolChain = chain
	}

	return false
}

// Recommend returns the best strategy for a given task context.
func (d *Distiller) Recommend(taskHint string) *StrategyEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var best *StrategyEntry
	var bestScore float64

	for pattern, strat := range d.strategies {
		if strat.SampleCount < d.minSamples {
			continue
		}
		relevance := patternRelevance(pattern, taskHint)
		score := strat.SuccessRate * relevance
		if score > bestScore {
			bestScore = score
			best = strat
		}
	}

	if best == nil {
		return nil
	}
	copied := *best
	return &copied
}

// FormatRecommendation generates a prompt hint from the best strategy.
func (d *Distiller) FormatRecommendation(taskHint string) string {
	strat := d.Recommend(taskHint)
	if strat == nil {
		return ""
	}

	return fmt.Sprintf("[Learned Strategy] For %s tasks, the tool sequence %s has %.0f%% success rate (%d samples).",
		strat.TaskPattern,
		strings.Join(strat.ToolChain, " → "),
		strat.SuccessRate*100,
		strat.SampleCount)
}

// Strategies returns all distilled strategies sorted by success rate.
func (d *Distiller) Strategies() []StrategyEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make([]StrategyEntry, 0, len(d.strategies))
	for _, s := range d.strategies {
		result = append(result, *s)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].SuccessRate > result[j].SuccessRate
	})
	return result
}

func extractToolChain(feedbacks []Feedback) []string {
	seen := make(map[string]bool)
	var chain []string
	for _, fb := range feedbacks {
		if fb.Success && !seen[fb.ToolName] {
			seen[fb.ToolName] = true
			chain = append(chain, fb.ToolName)
		}
	}
	return chain
}

func classifyTaskPattern(chain []string) string {
	if len(chain) == 0 {
		return "unknown"
	}

	hasRead := false
	hasWrite := false
	hasTest := false
	for _, tool := range chain {
		switch {
		case strings.Contains(tool, "read"):
			hasRead = true
		case strings.Contains(tool, "write") || strings.Contains(tool, "edit") || strings.Contains(tool, "patch"):
			hasWrite = true
		case strings.Contains(tool, "test"):
			hasTest = true
		}
	}

	switch {
	case hasRead && hasWrite && hasTest:
		return "implement_and_verify"
	case hasRead && hasWrite:
		return "code_modification"
	case hasRead && !hasWrite:
		return "code_analysis"
	case hasTest:
		return "testing"
	default:
		return "general"
	}
}

func patternRelevance(pattern, taskHint string) float64 {
	hint := strings.ToLower(taskHint)
	switch pattern {
	case "implement_and_verify":
		if strings.Contains(hint, "implement") || strings.Contains(hint, "add") || strings.Contains(hint, "feature") {
			return 1.0
		}
		return 0.5
	case "code_modification":
		if strings.Contains(hint, "fix") || strings.Contains(hint, "change") || strings.Contains(hint, "update") {
			return 1.0
		}
		return 0.5
	case "code_analysis":
		if strings.Contains(hint, "find") || strings.Contains(hint, "search") || strings.Contains(hint, "understand") {
			return 1.0
		}
		return 0.4
	case "testing":
		if strings.Contains(hint, "test") || strings.Contains(hint, "verify") {
			return 1.0
		}
		return 0.3
	}
	return 0.3
}

func computeSuccessRate(feedbacks []Feedback) float64 {
	if len(feedbacks) == 0 {
		return 0
	}
	successes := 0
	for _, fb := range feedbacks {
		if fb.Success {
			successes++
		}
	}
	return float64(successes) / float64(len(feedbacks))
}

func computeAvgDuration(feedbacks []Feedback) int {
	if len(feedbacks) == 0 {
		return 0
	}
	total := 0
	for _, fb := range feedbacks {
		total += fb.Duration
	}
	return total / len(feedbacks)
}
