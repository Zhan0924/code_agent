package orchestrator

import (
	"fmt"
	"strings"
	"sync"

	"github.com/agent/code_agent/internal/models"
)

// MetacognitiveState tracks the agent's self-awareness during a ReAct loop execution.
type MetacognitiveState struct {
	mu sync.Mutex

	Confidence      float64  // 0.0–1.0, rolling confidence based on recent outcomes
	UncertainAreas  []string // topics/files the agent is unsure about
	AssumptionsMade []string // explicit assumptions recorded during execution
	StuckScore      float64  // 0.0–1.0, how "stuck" the agent appears to be

	// internal tracking
	recentOutcomes []bool // sliding window of last N tool outcomes (true=success)
	windowSize     int
	totalSteps     int
	pivotCount     int // number of strategy changes
}

// NewMetacognitiveState creates a fresh metacognitive tracker.
func NewMetacognitiveState() *MetacognitiveState {
	return &MetacognitiveState{
		Confidence:     0.7,
		recentOutcomes: make([]bool, 0, 16),
		windowSize:     8,
	}
}

// RecordOutcome records a tool execution outcome and updates confidence.
func (m *MetacognitiveState) RecordOutcome(toolName string, success bool, isRepeat bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.totalSteps++
	m.recentOutcomes = append(m.recentOutcomes, success)
	if len(m.recentOutcomes) > m.windowSize {
		m.recentOutcomes = m.recentOutcomes[len(m.recentOutcomes)-m.windowSize:]
	}

	m.Confidence = m.computeConfidence()
	m.StuckScore = m.computeStuckScore(isRepeat)
}

// AddUncertainty records an area where the agent lacks confidence.
func (m *MetacognitiveState) AddUncertainty(area string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Check for duplicates
	for _, existing := range m.UncertainAreas {
		if existing == area {
			return
		}
	}
	m.UncertainAreas = append(m.UncertainAreas, area)
}

// AddAssumption records an explicit assumption the agent is making.
func (m *MetacognitiveState) AddAssumption(assumption string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AssumptionsMade = append(m.AssumptionsMade, assumption)
}

// RecordPivot notes that the agent changed strategy.
func (m *MetacognitiveState) RecordPivot() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pivotCount++
}

// NeedsReflection returns true when the metacognitive state suggests
// the agent should pause and self-evaluate, independent of the fixed 10-step interval.
func (m *MetacognitiveState) NeedsReflection() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Confidence < 0.3 || m.StuckScore > 0.7
}

// ShouldRequestClarification returns true when confidence is very low
// and the agent should consider asking the user for help.
func (m *MetacognitiveState) ShouldRequestClarification() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Confidence < 0.2 && m.totalSteps > 4
}

// AdaptiveReflectionMessage generates a context-aware reflection prompt
// based on the current metacognitive state.
func (m *MetacognitiveState) AdaptiveReflectionMessage(step, maxSteps int) *models.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	var parts []string
	remaining := maxSteps - step

	parts = append(parts, fmt.Sprintf(
		"[METACOGNITIVE CHECKPOINT — Step %d/%d, confidence=%.0f%%, stuck=%.0f%%]",
		step, maxSteps, m.Confidence*100, m.StuckScore*100))

	if m.StuckScore > 0.5 {
		parts = append(parts, "⚠️ You appear to be stuck. STOP repeating the same approach.")
		parts = append(parts, "Consider: (1) re-read the error messages carefully, (2) try a completely different strategy, (3) ask the user for clarification.")
	}

	if m.Confidence < 0.4 {
		parts = append(parts, "Your recent actions have low success rate. Before the next tool call:")
		parts = append(parts, "- State what you're trying to achieve in one sentence")
		parts = append(parts, "- Explain WHY you believe the next action will succeed")
		parts = append(parts, "- If unsure, say so explicitly rather than guessing")
	}

	if len(m.UncertainAreas) > 0 {
		parts = append(parts, fmt.Sprintf("Known uncertainties: %s", strings.Join(m.UncertainAreas, ", ")))
		parts = append(parts, "Address these uncertainties before proceeding with dependent work.")
	}

	if len(m.AssumptionsMade) > 0 && m.Confidence < 0.5 {
		parts = append(parts, fmt.Sprintf("Assumptions made: %s", strings.Join(m.AssumptionsMade, "; ")))
		parts = append(parts, "Verify these assumptions — one may be wrong.")
	}

	if remaining < 10 {
		parts = append(parts, fmt.Sprintf("⚠️ Only %d steps remaining. Focus on delivering a working result, not perfection.", remaining))
	}

	return &models.Message{
		Role:    models.RoleSystem,
		Content: strings.Join(parts, "\n"),
	}
}

func (m *MetacognitiveState) computeConfidence() float64 {
	if len(m.recentOutcomes) == 0 {
		return 0.7 // neutral prior
	}
	successes := 0
	for _, ok := range m.recentOutcomes {
		if ok {
			successes++
		}
	}
	return float64(successes) / float64(len(m.recentOutcomes))
}

func (m *MetacognitiveState) computeStuckScore(isRepeat bool) float64 {
	if len(m.recentOutcomes) < 3 {
		return 0.0
	}

	// Count consecutive recent failures
	consecutiveFails := 0
	for i := len(m.recentOutcomes) - 1; i >= 0; i-- {
		if !m.recentOutcomes[i] {
			consecutiveFails++
		} else {
			break
		}
	}

	score := float64(consecutiveFails) / float64(m.windowSize)
	if isRepeat {
		score += 0.2
	}
	if score > 1.0 {
		score = 1.0
	}
	return score
}
