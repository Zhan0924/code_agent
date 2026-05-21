package toollearn

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// Extractor analyzes collected feedback to identify patterns.
type Extractor struct {
	collector *Collector
	mu        sync.RWMutex
	patterns  map[string]*ToolPattern
	logger    *zap.Logger
}

// NewExtractor creates a pattern extractor backed by a collector.
func NewExtractor(collector *Collector, logger *zap.Logger) *Extractor {
	return &Extractor{
		collector: collector,
		patterns:  make(map[string]*ToolPattern),
		logger:    logger.With(zap.String("component", "toollearn.extractor")),
	}
}

// Analyze processes recent feedback and updates patterns for a tool.
func (e *Extractor) Analyze(toolName string) *ToolPattern {
	recent := e.collector.RecentFeedback(toolName, 50)
	if len(recent) < 3 {
		return nil
	}

	var successes, failures int
	var totalDuration int
	errorCounts := make(map[string]int)

	for _, fb := range recent {
		if fb.Success {
			successes++
		} else {
			failures++
			if fb.ErrorMsg != "" {
				errorCounts[fb.ErrorMsg]++
			}
		}
		totalDuration += fb.Duration
	}

	total := successes + failures
	pattern := &ToolPattern{
		ToolName:    toolName,
		FailureRate: float64(failures) / float64(total),
		AvgDuration: totalDuration / total,
		SampleSize:  total,
	}

	for errMsg, count := range errorCounts {
		if count >= 2 {
			pattern.CommonErrors = append(pattern.CommonErrors, errMsg)
		}
	}

	e.mu.Lock()
	e.patterns[toolName] = pattern
	e.mu.Unlock()

	return pattern
}

// GetPattern returns the cached pattern for a tool.
func (e *Extractor) GetPattern(toolName string) *ToolPattern {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.patterns[toolName]
}

// AnalyzeAll refreshes patterns for all tools seen in the buffer.
func (e *Extractor) AnalyzeAll() {
	seen := make(map[string]bool)
	recent := e.collector.RecentFeedback("", 500)
	for _, fb := range recent {
		seen[fb.ToolName] = true
	}
	for tool := range seen {
		e.Analyze(tool)
	}
}

// FailureSequences identifies repeated failure patterns (same tool failing N times in a row).
func (e *Extractor) FailureSequences(toolName string, minStreak int) []time.Time {
	recent := e.collector.RecentFeedback(toolName, 100)
	var streakStarts []time.Time
	streak := 0

	for _, fb := range recent {
		if !fb.Success {
			streak++
			if streak == minStreak {
				streakStarts = append(streakStarts, fb.CreatedAt)
			}
		} else {
			streak = 0
		}
	}
	return streakStarts
}
