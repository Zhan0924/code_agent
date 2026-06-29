package memory

import (
	"context"
	"fmt"
	"time"
)

// MarkDistilled marks the given episodic memories as consumed by the
// Distiller. We only persist this state in the cold tier (the source of
// truth); the hot tier's 24h TTL covers staleness implicitly — even if
// hot still has an unmarked copy of a distilled episode, RetrieveByQuery
// already filters episodic out, so the staleness has no observable effect
// on prompt-injected memory.
func (h *HybridStore) MarkDistilled(ctx context.Context, ids []string) error {
	if h.cold == nil || len(ids) == 0 {
		return nil
	}
	return h.cold.MarkDistilled(ctx, ids)
}

func (h *HybridStore) DeleteOldEpisodic(ctx context.Context, olderThan time.Duration) (int64, error) {
	if h.cold == nil {
		return 0, nil
	}
	return h.cold.DeleteOldEpisodic(ctx, olderThan)
}

// GetByID is the AUDIT-P2-5 explainability primitive: returns one memory
// straight from cold (source of truth) regardless of hot-tier TTL state.
// Returns (nil, nil) when the row does not exist so the HTTP layer can
// translate that into 404 without inventing sentinel errors. Cold-only
// because the hot copy's score/access_count can lag the cold copy after
// a Touch flush — the support engineer running the audit wants the
// authoritative number.
func (h *HybridStore) GetByID(ctx context.Context, id string) (*Memory, error) {
	if h.cold == nil {
		return nil, nil
	}
	return h.cold.GetByID(ctx, id)
}

// DeleteByUser removes all memories belonging to a specific user across both tiers.
// It also publishes a "deleted_user" event to the blackboard.
func (h *HybridStore) DeleteByUser(ctx context.Context, userID string) (int64, error) {
	if userID == "" {
		return 0, nil
	}

	var totalDeleted int64

	if h.cold != nil {
		if deleted, err := h.cold.DeleteByUser(ctx, userID); err != nil {
			return 0, fmt.Errorf("cold tier delete failed: %w", err)
		} else {
			totalDeleted += deleted
		}
	}

	if h.hot != nil {
		if deleted, err := h.hot.DeleteByUser(ctx, userID); err != nil {
			return totalDeleted, fmt.Errorf("hot tier delete failed: %w", err)
		} else {
			totalDeleted += deleted
		}
	}

	if h.blackboard != nil {
		// Broadcast deletion so any active listeners can drop caches
		dummyMem := &Memory{
			UserID: userID,
		}
		_ = h.blackboard.Publish(ctx, "deleted_user", dummyMem)
	}

	return totalDeleted, nil
}

// ListActiveDistillTenants delegates to cold (PG owns the GROUP BY index).
// Hot tier doesn't store cross-tenant aggregates, so there's nothing to
// fall back to: with cold==nil we return nil/nil rather than ranging over
// hot SCAN keys (the per-tenant counts on hot would skew toward the 24h
// window, defeating the "actually distill once per day" semantics).
func (h *HybridStore) ListActiveDistillTenants(ctx context.Context, minEpisodic, limit int) ([]TenantRef, error) {
	if h.cold == nil {
		return nil, nil
	}
	return h.cold.ListActiveDistillTenants(ctx, minEpisodic, limit)
}

// Touch updates access metadata in cold store. Prefer the implicit
// path: every Retrieve / RetrieveByType / List enqueues its hits into
// the access batcher, so callers normally never need to call Touch
// directly. Kept exported for legacy callers + ad-hoc "I just merged
// this" use cases.
func (h *HybridStore) Touch(id string) error {
	if h.cold != nil {
		return h.cold.Touch(id)
	}
	return nil
}

// BoostScoreBatch increases the score of specified memories by the given amount.
func (h *HybridStore) BoostScoreBatch(ctx context.Context, refs []TouchRef, boost float64) error {
	if len(refs) == 0 {
		return nil
	}
	var errs []error

	// Cold store only needs IDs
	if h.cold != nil {
		ids := make([]string, len(refs))
		for i, r := range refs {
			ids[i] = r.ID
		}
		if err := h.cold.BoostScoreBatch(ctx, ids, boost); err != nil {
			errs = append(errs, err)
		}
	}

	// Hot store needs full TouchRefs
	if h.hot != nil {
		if err := h.hot.BoostScoreBatch(ctx, refs, boost); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("boost score errors: %v", errs)
	}
	return nil
}

// TouchBatch is the id-only public API kept for MemoryStore interface
// compatibility. Updates cold only — the public surface has no
// (user, project) context to reconstruct the hot key. The batched
// read-path goes through flushTouches with full TouchRefs, which
// keeps hot in sync.
func (h *HybridStore) TouchBatch(ctx context.Context, ids []string) error {
	if h.cold == nil || len(ids) == 0 {
		return nil
	}
	return h.cold.TouchBatch(ctx, ids)
}
