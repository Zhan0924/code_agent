package memory

import (
	"context"
	"sort"
	"time"

	"go.uber.org/zap"
)

// Embedder generates vector embeddings from text.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// HybridStore combines Redis (hot) and PostgreSQL (cold) memory stores.
// Reads check Redis first, falling back to PG. Writes go to both.
type HybridStore struct {
	hot      *RedisHot
	cold     *PGCold
	embedder Embedder
	logger   *zap.Logger
}

// NewHybridStore creates a hybrid memory store.
func NewHybridStore(hot *RedisHot, cold *PGCold, logger *zap.Logger) *HybridStore {
	return &HybridStore{
		hot:    hot,
		cold:   cold,
		logger: logger.With(zap.String("component", "memory.hybrid")),
	}
}

// SetEmbedder injects an embedder for semantic operations.
func (h *HybridStore) SetEmbedder(e Embedder) {
	h.embedder = e
}

// embedText generates an embedding for a single text, returning nil on failure.
func (h *HybridStore) embedText(ctx context.Context, text string) []float32 {
	if h.embedder == nil {
		h.logger.Debug("embedder is nil, skipping embedding generation")
		return nil
	}
	vecs, err := h.embedder.Embed(ctx, []string{text})
	if err != nil || len(vecs) == 0 {
		h.logger.Debug("embedding failed", zap.Error(err))
		return nil
	}
	return vecs[0]
}

// Store writes a memory to both hot and cold stores.
// Generates embedding if an embedder is available, and resolves conflicts
// with existing memories before inserting.
func (h *HybridStore) Store(ctx context.Context, m *Memory) error {
	if len(m.Embedding) == 0 {
		m.Embedding = h.embedText(ctx, m.Content)
	}

	// Conflict resolution: check if a highly similar memory already exists
	if h.cold != nil && len(m.Embedding) > 0 {
		candidates, err := h.cold.RetrieveByVector(m.Embedding, m.UserID, m.ProjectID, 3)
		if err == nil && len(candidates) > 0 {
			resolver := NewConflictResolver(h.cold)
			conflicts := resolver.FindConflicts(m, candidates)
			if len(conflicts) > 0 {
				resolved := resolver.Resolve(&conflicts[0], m)
				if err := h.cold.Update(resolved); err != nil {
					h.logger.Debug("conflict resolution update failed", zap.Error(err))
				} else {
					if h.hot != nil {
						_ = h.hot.Store(ctx, resolved)
					}
					return nil
				}
			}
		}
	}

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

// Retrieve searches memories using semantic similarity when possible,
// falling back to text-based search.
func (h *HybridStore) Retrieve(ctx context.Context, userID, projectID, query string, limit int) ([]Memory, error) {
	queryEmbedding := h.embedText(ctx, query)

	// Try hot layer semantic search first
	if h.hot != nil && queryEmbedding != nil {
		memories, err := h.hot.RetrieveByQuery(ctx, userID, projectID, queryEmbedding, limit)
		if err == nil && len(memories) >= limit {
			return memories, nil
		}
	} else if h.hot != nil {
		memories, err := h.hot.Retrieve(ctx, userID, projectID, limit)
		if err == nil && len(memories) >= limit {
			return memories, nil
		}
	}

	// Fall back to cold layer
	if h.cold != nil {
		if queryEmbedding != nil {
			return h.cold.RetrieveByVector(queryEmbedding, userID, projectID, limit)
		}
		return h.cold.Retrieve(userID, projectID, query, limit)
	}
	return nil, nil
}

// List returns memories for (userID, projectID) without semantic ranking.
// Merges hot + cold by ID (hot wins on conflict), sorted by LastAccessedAt desc.
// Intended for UI inspection where the full enumeration is wanted, not relevance.
// Returns (memories, hotCount, coldCount).
func (h *HybridStore) List(ctx context.Context, userID, projectID string, limit int) ([]Memory, int, int, error) {
	var hotMems, coldMems []Memory
	if h.hot != nil {
		ms, err := h.hot.Retrieve(ctx, userID, projectID, limit)
		if err != nil {
			h.logger.Debug("hot list failed", zap.Error(err))
		} else {
			hotMems = ms
		}
	}
	if h.cold != nil {
		ms, err := h.cold.Retrieve(userID, projectID, "", limit)
		if err != nil {
			h.logger.Debug("cold list failed", zap.Error(err))
		} else {
			coldMems = ms
		}
	}

	seen := make(map[string]struct{}, len(hotMems)+len(coldMems))
	merged := make([]Memory, 0, len(hotMems)+len(coldMems))
	for _, m := range hotMems {
		if _, dup := seen[m.ID]; dup {
			continue
		}
		seen[m.ID] = struct{}{}
		merged = append(merged, m)
	}
	for _, m := range coldMems {
		if _, dup := seen[m.ID]; dup {
			continue
		}
		seen[m.ID] = struct{}{}
		merged = append(merged, m)
	}

	sort.Slice(merged, func(i, j int) bool {
		return merged[i].LastAccessedAt.After(merged[j].LastAccessedAt)
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, len(hotMems), len(coldMems), nil
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
