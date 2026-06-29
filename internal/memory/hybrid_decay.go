package memory

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// SetDemoteThreshold configures the P1 #8 hot-eviction floor used by
// the Decay path. Zero (default) keeps the legacy behavior of "SET with
// reduced score" — no DELs from hot during decay. Positive value
// triggers DEL when an entry's score crosses below the threshold this
// iteration. Must be > 0.01 to interact meaningfully with the existing
// score floor.
func (h *HybridStore) SetDemoteThreshold(t float64) {
	h.demoteThreshold = t
}

// Decay reduces scores for old memories in both hot and cold stores.
// Returns the cold-tier mutation count (hot is best-effort).
//
// Hot decay path (P1 #6 + P1 #8):
//
// Before P1 #6, hot.Decay did SCAN `memory:*` on every tick. We now
// ask cold "which tenants even have stale entries?" and only SCAN
// their sub-namespaces. P1 #8 additionally passes the demoteThreshold
// so hot DELs entries whose post-decay score crosses below the
// threshold — instead of caching low-signal entries until TTL expires.
// If cold == nil we fall back to the legacy whole-DB scan (still
// bounded by hotScanLimit, so it can't go runaway).
func (h *HybridStore) Decay(olderThan time.Duration, factor float64) (int, error) {
	var coldCount int
	var err error
	if h.cold != nil {
		coldCount, err = h.cold.Decay(olderThan, factor)
	}
	if h.hot != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if h.cold != nil {
			// Tenant-sliced path: ask cold who has stale data, then
			// run hot decay only against those (user, project) prefixes.
			// 200 is a sane default — at 24h decay cadence on a single
			// Redis instance this is plenty; operators with > 200
			// active tenants should batch or stagger their ticks.
			tenants, listErr := h.cold.ListActiveDecayTenants(ctx, olderThan, 200)
			if listErr != nil {
				h.logger.Warn("hot decay tenant discovery failed; skipping hot",
					zap.Error(listErr))
			} else if len(tenants) > 0 {
				if _, hotErr := h.hot.DecayTenants(ctx, tenants, olderThan, factor, h.demoteThreshold); hotErr != nil {
					h.logger.Debug("hot decay had per-tenant failures",
						zap.Int("tenants", len(tenants)), zap.Error(hotErr))
				}
			}
		} else {
			// Fallback: no cold store → walk all hot keys with the
			// scan-budget-capped path. Mostly relevant for tests &
			// pure-hot dev environments.
			if _, hotErr := h.hot.Decay(ctx, olderThan, factor, h.demoteThreshold); hotErr != nil {
				h.logger.Debug("hot decay (fallback) failed", zap.Error(hotErr))
			}
		}
	}
	return coldCount, err
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
