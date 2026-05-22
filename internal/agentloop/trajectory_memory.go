package agentloop

import (
	"fmt"
	"strings"
	"sync"
)

const (
	maxTrajectories     = 50
	maxToolsPerEpisode  = 20
	trajectoryTopK      = 3
)

// TrajectoryEntry records a successful tool sequence for a task type.
type TrajectoryEntry struct {
	Intent    string   // task intent/category (e.g., "code_fix", "code_generate")
	Tools     []string // ordered list of tools used
	StepCount int
	Success   bool
}

// TrajectoryMemory stores successful execution patterns for retrieval.
type TrajectoryMemory struct {
	mu      sync.RWMutex
	entries []TrajectoryEntry
}

// NewTrajectoryMemory creates a trajectory store.
func NewTrajectoryMemory() *TrajectoryMemory {
	return &TrajectoryMemory{
		entries: make([]TrajectoryEntry, 0, maxTrajectories),
	}
}

// Record stores a completed trajectory.
func (tm *TrajectoryMemory) Record(intent string, tools []string, success bool) {
	if len(tools) == 0 {
		return
	}
	if len(tools) > maxToolsPerEpisode {
		tools = tools[:maxToolsPerEpisode]
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.entries = append(tm.entries, TrajectoryEntry{
		Intent:    intent,
		Tools:     tools,
		StepCount: len(tools),
		Success:   success,
	})

	if len(tm.entries) > maxTrajectories {
		tm.entries = tm.entries[len(tm.entries)-maxTrajectories:]
	}
}

// Retrieve finds successful trajectories matching the given intent.
func (tm *TrajectoryMemory) Retrieve(intent string) []TrajectoryEntry {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	var matches []TrajectoryEntry
	for i := len(tm.entries) - 1; i >= 0 && len(matches) < trajectoryTopK; i-- {
		e := tm.entries[i]
		if e.Success && e.Intent == intent {
			matches = append(matches, e)
		}
	}
	return matches
}

// FormatHint generates a system message with historical tool patterns for the given intent.
func (tm *TrajectoryMemory) FormatHint(intent string) string {
	matches := tm.Retrieve(intent)
	if len(matches) == 0 {
		return ""
	}

	var lines []string
	lines = append(lines, "[TRAJECTORY HINT] 历史上类似任务的成功工具序列：")
	for i, m := range matches {
		lines = append(lines, fmt.Sprintf("  %d. %s（%d 步）", i+1, strings.Join(m.Tools, " → "), m.StepCount))
	}
	lines = append(lines, "可参考上述模式，但应根据当前具体情况灵活调整。")
	return strings.Join(lines, "\n")
}
