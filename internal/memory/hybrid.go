package memory

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// HybridStore combines Redis (hot) and PostgreSQL (cold) memory stores.
// Reads check Redis first, falling back to PG. Writes go to both.
type HybridStore struct {
	hot    *RedisHot
	cold   *PGCold
	logger *zap.Logger
}

// NewHybridStore creates a hybrid memory store.
func NewHybridStore(hot *RedisHot, cold *PGCold, logger *zap.Logger) *HybridStore {
	return &HybridStore{
		hot:    hot,
		cold:   cold,
		logger: logger.With(zap.String("component", "memory.hybrid")),
	}
}

// Store writes a memory to both hot and cold stores.
func (h *HybridStore) Store(ctx context.Context, m *Memory) error {
	if h.hot != nil {
		if err := h.hot.Store(ctx, m); err != nil {
			h.logger.Debug("hot store write failed", zap.Error(err))
		}
	}
	if h.cold != nil {
		if err := h.cold.Store(m); err != nil {
			return err
		}
	}
	return nil
}

// Retrieve searches hot store first, falls back to cold store.
func (h *HybridStore) Retrieve(ctx context.Context, userID, projectID, query string, limit int) ([]Memory, error) {
	if h.hot != nil {
		memories, err := h.hot.Retrieve(ctx, userID, projectID, limit)
		if err == nil && len(memories) >= limit {
			return memories, nil
		}
	}

	if h.cold != nil {
		return h.cold.Retrieve(userID, projectID, query, limit)
	}
	return nil, nil
}

// Touch updates access metadata in cold store.
func (h *HybridStore) Touch(id string) error {
	if h.cold != nil {
		return h.cold.Touch(id)
	}
	return nil
}

// Decay reduces scores for old memories in cold store.
func (h *HybridStore) Decay(olderThan time.Duration, factor float64) (int, error) {
	if h.cold != nil {
		return h.cold.Decay(olderThan, factor)
	}
	return 0, nil
}

// Promote moves a cold memory to hot (Redis) for faster access.
func (h *HybridStore) Promote(ctx context.Context, m *Memory) error {
	if h.hot != nil {
		return h.hot.Store(ctx, m)
	}
	return nil
}

// Demote removes a memory from hot store (it remains in cold).
func (h *HybridStore) Demote(ctx context.Context, m *Memory) error {
	if h.hot != nil {
		return h.hot.Delete(ctx, m.UserID, m.ProjectID, m.ID)
	}
	return nil
}
