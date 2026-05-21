package toollearn

import (
	"sort"
	"sync"
	"time"
)

// AdaptivePolicy dynamically adjusts tool ordering and provides context-aware
// recommendations based on historical success rates and tool sequence patterns.
type AdaptivePolicy struct {
	mu        sync.RWMutex
	collector *Collector

	// Per-tool effectiveness scores (higher = more reliable)
	scores map[string]*toolScore

	// Sequence patterns: "toolA→toolB" → success rate
	sequences map[string]*sequenceStats

	// Configuration
	decayFactor  float64
	minSamples   int
	windowSize   int
	lastAnalysis time.Time
}

type toolScore struct {
	SuccessRate   float64
	AvgDuration   time.Duration
	RecentTrend   float64 // positive = improving, negative = degrading
	SampleCount   int
	LastUpdated   time.Time
}

type sequenceStats struct {
	SuccessCount int
	TotalCount   int
	AvgDuration  time.Duration
}

// NewAdaptivePolicy creates a policy that learns from tool execution history.
func NewAdaptivePolicy(collector *Collector) *AdaptivePolicy {
	return &AdaptivePolicy{
		collector:    collector,
		scores:       make(map[string]*toolScore),
		sequences:    make(map[string]*sequenceStats),
		decayFactor:  0.9,
		minSamples:   3,
		windowSize:   50,
		lastAnalysis: time.Time{},
	}
}

// Update refreshes the policy from recent feedback. Call periodically or after
// a batch of tool executions.
func (p *AdaptivePolicy) Update() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.collector.mu.Lock()
	buffer := make([]Feedback, len(p.collector.buffer))
	copy(buffer, p.collector.buffer)
	p.collector.mu.Unlock()

	if len(buffer) == 0 {
		return
	}

	p.updateScores(buffer)
	p.updateSequences(buffer)
	p.lastAnalysis = time.Now()
}

func (p *AdaptivePolicy) updateScores(buffer []Feedback) {
	// Group by tool, take last windowSize entries per tool
	byTool := make(map[string][]Feedback)
	for _, fb := range buffer {
		byTool[fb.ToolName] = append(byTool[fb.ToolName], fb)
	}

	for toolName, feedbacks := range byTool {
		if len(feedbacks) < p.minSamples {
			continue
		}

		// Take most recent entries
		if len(feedbacks) > p.windowSize {
			feedbacks = feedbacks[len(feedbacks)-p.windowSize:]
		}

		var successes int
		var totalDuration time.Duration
		for _, fb := range feedbacks {
			if fb.Success {
				successes++
			}
			totalDuration += time.Duration(fb.Duration) * time.Millisecond
		}

		rate := float64(successes) / float64(len(feedbacks))
		avgDur := totalDuration / time.Duration(len(feedbacks))

		// Compute trend: compare first half vs second half success rates
		var trend float64
		if len(feedbacks) >= 6 {
			mid := len(feedbacks) / 2
			var firstHalf, secondHalf int
			for _, fb := range feedbacks[:mid] {
				if fb.Success {
					firstHalf++
				}
			}
			for _, fb := range feedbacks[mid:] {
				if fb.Success {
					secondHalf++
				}
			}
			firstRate := float64(firstHalf) / float64(mid)
			secondRate := float64(secondHalf) / float64(len(feedbacks)-mid)
			trend = secondRate - firstRate
		}

		p.scores[toolName] = &toolScore{
			SuccessRate: rate,
			AvgDuration: avgDur,
			RecentTrend: trend,
			SampleCount: len(feedbacks),
			LastUpdated: time.Now(),
		}
	}
}

func (p *AdaptivePolicy) updateSequences(buffer []Feedback) {
	// Identify consecutive tool pairs within the same session
	bySession := make(map[string][]Feedback)
	for _, fb := range buffer {
		bySession[fb.SessionID] = append(bySession[fb.SessionID], fb)
	}

	for _, feedbacks := range bySession {
		if len(feedbacks) < 2 {
			continue
		}
		// Sort by time
		sort.Slice(feedbacks, func(i, j int) bool {
			return feedbacks[i].CreatedAt.Before(feedbacks[j].CreatedAt)
		})

		for i := 0; i < len(feedbacks)-1; i++ {
			key := feedbacks[i].ToolName + "→" + feedbacks[i+1].ToolName
			stats, ok := p.sequences[key]
			if !ok {
				stats = &sequenceStats{}
				p.sequences[key] = stats
			}
			stats.TotalCount++
			if feedbacks[i+1].Success {
				stats.SuccessCount++
			}
			stats.AvgDuration = time.Duration(
				(int64(stats.AvgDuration)*int64(stats.TotalCount-1) + int64(feedbacks[i+1].Duration)*int64(time.Millisecond)) / int64(stats.TotalCount),
			)
		}
	}
}

// RankTools reorders tool names by their learned effectiveness for the current context.
// Tools with higher success rates and positive trends are ranked first.
// Unknown tools retain their original relative order.
func (p *AdaptivePolicy) RankTools(toolNames []string, lastTool string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	type ranked struct {
		name     string
		score    float64
		origIdx  int
	}

	items := make([]ranked, len(toolNames))
	for i, name := range toolNames {
		items[i] = ranked{name: name, origIdx: i}

		ts, ok := p.scores[name]
		if !ok || ts.SampleCount < p.minSamples {
			items[i].score = 0.5 // neutral score for unknown tools
			continue
		}

		// Base score from success rate
		items[i].score = ts.SuccessRate

		// Boost from positive trend
		items[i].score += ts.RecentTrend * 0.1

		// Sequence bonus: if this tool follows lastTool with high success
		if lastTool != "" {
			key := lastTool + "→" + name
			if seq, exists := p.sequences[key]; exists && seq.TotalCount >= p.minSamples {
				seqRate := float64(seq.SuccessCount) / float64(seq.TotalCount)
				items[i].score += (seqRate - 0.5) * 0.2
			}
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].score > items[j].score
	})

	result := make([]string, len(items))
	for i, item := range items {
		result[i] = item.name
	}
	return result
}

// SuggestNext returns the best tool to use after the given tool, based on
// learned sequence patterns. Returns empty string if no strong suggestion.
func (p *AdaptivePolicy) SuggestNext(lastTool string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestTool string
	var bestRate float64

	for key, stats := range p.sequences {
		if stats.TotalCount < p.minSamples {
			continue
		}
		// Parse "toolA→toolB"
		for i := 0; i < len(key)-3; i++ {
			if key[i] == 0xe2 && key[i+1] == 0x86 && key[i+2] == 0x92 { // UTF-8 for →
				from := key[:i]
				to := key[i+3:]
				if from == lastTool {
					rate := float64(stats.SuccessCount) / float64(stats.TotalCount)
					if rate > bestRate && rate > 0.7 {
						bestRate = rate
						bestTool = to
					}
				}
				break
			}
		}
	}
	return bestTool
}

// GetToolScore returns the current effectiveness score for a tool.
// Returns nil if the tool hasn't been seen enough times.
func (p *AdaptivePolicy) GetToolScore(toolName string) *toolScore {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s, ok := p.scores[toolName]
	if !ok || s.SampleCount < p.minSamples {
		return nil
	}
	copied := *s
	return &copied
}

// FormatContextHint generates a brief system-prompt hint summarizing
// tool reliability for the LLM. Only includes tools with notable patterns.
func (p *AdaptivePolicy) FormatContextHint(lastTool string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var hints []string

	// Flag unreliable tools
	for name, ts := range p.scores {
		if ts.SampleCount < 5 {
			continue
		}
		if ts.SuccessRate < 0.4 {
			hints = append(hints, name+" has low reliability ("+
				formatPercent(ts.SuccessRate)+" success) — consider alternatives")
		}
		if ts.RecentTrend < -0.3 {
			hints = append(hints, name+" is degrading — recent calls fail more often")
		}
	}

	// Suggest next tool based on sequence
	if lastTool != "" {
		if next := p.suggestNextLocked(lastTool); next != "" {
			hints = append(hints, "After "+lastTool+", "+next+" typically succeeds")
		}
	}

	if len(hints) == 0 {
		return ""
	}

	result := "[Tool Learning Insights]\n"
	for _, h := range hints {
		result += "- " + h + "\n"
	}
	return result
}

func (p *AdaptivePolicy) suggestNextLocked(lastTool string) string {
	var bestTool string
	var bestRate float64

	for key, stats := range p.sequences {
		if stats.TotalCount < p.minSamples {
			continue
		}
		for i := 0; i < len(key)-3; i++ {
			if key[i] == 0xe2 && key[i+1] == 0x86 && key[i+2] == 0x92 {
				from := key[:i]
				to := key[i+3:]
				if from == lastTool {
					rate := float64(stats.SuccessCount) / float64(stats.TotalCount)
					if rate > bestRate && rate > 0.7 {
						bestRate = rate
						bestTool = to
					}
				}
				break
			}
		}
	}
	return bestTool
}

func formatPercent(f float64) string {
	i := int(f * 100)
	return string(rune('0'+i/10)) + string(rune('0'+i%10)) + "%"
}
