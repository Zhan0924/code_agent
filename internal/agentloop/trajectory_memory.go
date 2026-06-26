package agentloop

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

const (
	maxTrajectories    = 50
	maxToolsPerEpisode = 20
	trajectoryTopK     = 3
)

// TrajectoryEntry records a successful tool sequence for a task type.
type TrajectoryEntry struct {
	Intent    string   // task intent/category (e.g., "code_fix", "code_generate")
	Tools     []string // ordered list of tools used
	StepCount int
	Success   bool
}

// TrajectoryStore abstracts the persistence layer for execution
// trajectories. Two implementations exist:
//
//   - TrajectoryMemory (in-process, FIFO ring buffer) — the default,
//     used in tests and single-process deployments. Zero deps.
//   - PGTrajectoryStore (in internal/agentloop/pg_trajectory_store.go)
//     — persists across restarts and enables semantic ("intent
//     embedding") recall, so similar-but-not-identical intents still
//     hit the cached pattern.
//
// Both Record and Retrieve take context so PG calls can honour
// per-request deadlines; the in-memory implementation ignores ctx and
// never returns an error.
type TrajectoryStore interface {
	Record(ctx context.Context, intent string, tools []string, success bool) error
	Retrieve(ctx context.Context, intent string, limit int) ([]TrajectoryEntry, error)
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

// Record stores a completed trajectory. ctx is ignored (in-memory, no
// I/O); error is always nil — both kept on the signature to satisfy
// TrajectoryStore.
func (tm *TrajectoryMemory) Record(_ context.Context, intent string, tools []string, success bool) error {
	if len(tools) == 0 {
		return nil
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
	return nil
}

// Retrieve finds successful trajectories matching the given intent.
// Falls back to most-recent-first when limit≤0 the default trajectoryTopK applies.
func (tm *TrajectoryMemory) Retrieve(_ context.Context, intent string, limit int) ([]TrajectoryEntry, error) {
	if limit <= 0 {
		limit = trajectoryTopK
	}

	tm.mu.RLock()
	defer tm.mu.RUnlock()

	matches := make([]TrajectoryEntry, 0, limit)
	for i := len(tm.entries) - 1; i >= 0 && len(matches) < limit; i-- {
		e := tm.entries[i]
		if e.Success && e.Intent == intent {
			matches = append(matches, e)
		}
	}
	return matches, nil
}

// FormatTrajectoryHint renders successful tool sequences for the given
// intent into a one-shot system message hint. Returns "" when no usable
// trajectories exist or the store errors (errors are silenced because
// hints are best-effort prompt enrichment, not core behavior).
//
// Lives at package level (not on TrajectoryStore) so PGTrajectoryStore
// doesn't need to drag prompt-string formatting concerns into the SQL
// layer — formatting is a presentation concern, recall is a data concern.
func FormatTrajectoryHint(ctx context.Context, store TrajectoryStore, intent string) string {
	if store == nil {
		return ""
	}
	matches, err := store.Retrieve(ctx, intent, trajectoryTopK)
	if err != nil || len(matches) == 0 {
		return ""
	}

	lines := make([]string, 0, len(matches)+2)
	lines = append(lines, "[TRAJECTORY HINT] 历史上类似任务的成功工具序列：")
	for i, m := range matches {
		lines = append(lines, fmt.Sprintf("  %d. %s（%d 步）", i+1, strings.Join(m.Tools, " → "), m.StepCount))
	}
	lines = append(lines, "可参考上述模式，但应根据当前具体情况灵活调整。")
	return strings.Join(lines, "\n")
}

// FormatHint is kept as a thin wrapper on TrajectoryMemory for
// backward-compatibility with older call sites that did not yet have
// access to a context.Context. New code should call
// FormatTrajectoryHint directly.
func (tm *TrajectoryMemory) FormatHint(intent string) string {
	return FormatTrajectoryHint(context.Background(), tm, intent)
}
