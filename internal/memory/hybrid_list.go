package memory

import (
	"context"
	"sort"

	"github.com/agent/code_agent/internal/metrics"
	"go.uber.org/zap"
)

// List returns memories for (userID, projectID) without semantic ranking.
// Merges hot + cold by ID (hot wins on conflict), sorted by LastAccessedAt desc.
// Intended for UI inspection where the full enumeration is wanted, not relevance.
// Returns (memories, hotCount, coldCount).
func (h *HybridStore) List(ctx context.Context, userID, projectID string, limit int) ([]Memory, int, int, error) {
	var hotMems, coldMems []Memory
	if h.hot != nil {
		ms, err := h.hot.Retrieve(ctx, userID, projectID, limit)
		if err != nil {
			metrics.MemoryFailuresTotal.WithLabelValues("hot", "list", "warn").Inc()
			h.logger.Warn("hot list failed",
				zap.String("user_id", userID),
				zap.Error(err))
		} else {
			hotMems = ms
		}
	}
	if h.cold != nil {
		ms, err := h.cold.Retrieve(userID, projectID, "", limit)
		if err != nil {
			metrics.MemoryFailuresTotal.WithLabelValues("cold", "list", "error").Inc()
			h.logger.Error("cold list failed (PG query error)",
				zap.String("user_id", userID),
				zap.Error(err))
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
	h.enqueueTouches(merged)
	return merged, len(hotMems), len(coldMems), nil
}

// ListByType returns memories for (userID, projectID) filtered to a single
// MemoryType, no query embedding required. Used by the Distiller to pull
// recent episodic memories for periodic consolidation.
//
// Episodic path goes through PGCold.ListEpisodicUndistilled, which only
// returns rows where distilled_at IS NULL — this is the contract that
// prevents the Distiller from consuming the same episode twice across
// successive ticks. Without it, every tick would produce a fresh
// semantic memory consolidating the same source episodes, polluting the
// store with N near-duplicates after N ticks.
//
// Non-episodic paths fall back to the List() over-fetch + client filter
// approach because (a) other types are typically retrieved by query, not
// listed wholesale, and (b) the volume is bounded.
func (h *HybridStore) ListByType(ctx context.Context, userID, projectID string, memType MemoryType, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 50
	}
	if memType == MemoryTypeEpisodic && h.cold != nil {
		return h.cold.ListEpisodicUndistilled(ctx, userID, projectID, limit)
	}
	all, _, _, err := h.List(ctx, userID, projectID, limit*4)
	if err != nil {
		return nil, err
	}
	out := make([]Memory, 0, limit)
	for _, m := range all {
		if m.Type != memType {
			continue
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
