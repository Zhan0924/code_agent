package planner

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// ProgressTracker monitors execution progress across sub-goals.
type ProgressTracker struct {
	mu             sync.RWMutex
	subGoals       map[string]*trackedGoal
	consecutiveFails map[string]int
	logger         *zap.Logger
}

type trackedGoal struct {
	SubGoal   SubGoal
	StartedAt *time.Time
	DoneAt    *time.Time
	Attempts  int
}

// NewProgressTracker creates a progress tracker.
func NewProgressTracker(logger *zap.Logger) *ProgressTracker {
	return &ProgressTracker{
		subGoals:         make(map[string]*trackedGoal),
		consecutiveFails: make(map[string]int),
		logger:           logger.With(zap.String("component", "planner.progress_tracker")),
	}
}

// Track registers sub-goals for monitoring.
func (pt *ProgressTracker) Track(goals []SubGoal) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	for _, g := range goals {
		pt.subGoals[g.ID] = &trackedGoal{SubGoal: g}
	}
}

// MarkStarted records that a sub-goal has begun execution.
func (pt *ProgressTracker) MarkStarted(id string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if tg, ok := pt.subGoals[id]; ok {
		now := time.Now()
		tg.StartedAt = &now
		tg.SubGoal.Status = StepRunning
		tg.Attempts++
	}
}

// MarkCompleted records successful completion of a sub-goal.
func (pt *ProgressTracker) MarkCompleted(id string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if tg, ok := pt.subGoals[id]; ok {
		now := time.Now()
		tg.DoneAt = &now
		tg.SubGoal.Status = StepCompleted
		pt.consecutiveFails[id] = 0
	}
}

// MarkFailed records a failure and returns true if replanning is needed (3+ consecutive failures).
func (pt *ProgressTracker) MarkFailed(id string) bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.consecutiveFails[id]++
	if tg, ok := pt.subGoals[id]; ok {
		tg.SubGoal.Status = StepFailed
	}
	needsReplan := pt.consecutiveFails[id] >= 3
	if needsReplan {
		pt.logger.Warn("sub-goal needs replanning after 3 consecutive failures",
			zap.String("sub_goal_id", id))
	}
	return needsReplan
}

// Progress returns overall completion percentage.
func (pt *ProgressTracker) Progress() float64 {
	pt.mu.RLock()
	defer pt.mu.RUnlock()
	if len(pt.subGoals) == 0 {
		return 0
	}
	completed := 0
	for _, tg := range pt.subGoals {
		if tg.SubGoal.Status == StepCompleted || tg.SubGoal.Status == StepSkipped {
			completed++
		}
	}
	return float64(completed) / float64(len(pt.subGoals))
}

// PendingGoals returns sub-goals that haven't started yet.
func (pt *ProgressTracker) PendingGoals() []SubGoal {
	pt.mu.RLock()
	defer pt.mu.RUnlock()
	var pending []SubGoal
	for _, tg := range pt.subGoals {
		if tg.SubGoal.Status == StepPending {
			pending = append(pending, tg.SubGoal)
		}
	}
	return pending
}

// NeedsReplanning returns IDs of sub-goals that have failed 3+ times.
func (pt *ProgressTracker) NeedsReplanning() []string {
	pt.mu.RLock()
	defer pt.mu.RUnlock()
	var ids []string
	for id, count := range pt.consecutiveFails {
		if count >= 3 {
			ids = append(ids, id)
		}
	}
	return ids
}
