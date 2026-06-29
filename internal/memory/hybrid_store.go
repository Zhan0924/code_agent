package memory

import (
	"context"
	"time"

	"github.com/agent/code_agent/internal/metrics"
	"go.uber.org/zap"
)

// Store writes a memory to both hot and cold stores with best-effort
// consistency:
//
//   - Cold (source of truth) is written first; failure aborts and we do NOT
//     leave a hot-only ghost record (the previous "hot has it, cold lost it,
//     hot TTL expires → silent disappearance" bug).
//   - Hot write failure is non-fatal — hot is a cache, cold has the truth.
//     A future Retrieve will repopulate hot via cache-aside semantics.
//   - blackboard.Publish runs on *both* the conflict-merge branch and the
//     new-insert branch (previous code skipped publishing on merge, which
//     broke multi-agent state sync on memory updates).
func (h *HybridStore) Store(ctx context.Context, m *Memory) (retErr error) {
	// Apply PII masking (shield against unmasked tool injection)
	m.Content = h.masker.Mask(m.Content)

	start := time.Now()
	// Tier label is decided at exit (hybrid when both tiers ran, cold-only
	// otherwise). Status defaults to ok and flips to err on early return.
	tier := "hybrid"
	if h.hot == nil {
		tier = "cold"
	} else if h.cold == nil {
		tier = "hot"
	}
	memType := string(m.Type)
	defer func() {
		status := "ok"
		if retErr != nil {
			status = "err"
		}
		metrics.MemoryStoreTotal.WithLabelValues(tier, status, memType).Inc()
		metrics.MemoryStoreDuration.WithLabelValues(tier, status).Observe(time.Since(start).Seconds())
	}()

	if len(m.Embedding) == 0 {
		m.Embedding = h.embedText(ctx, m.Content)
	}

	// Conflict resolution: check if a highly similar memory already exists.
	//
	// dedupOversample over-fetches candidates so we can not only merge
	// the new memory but also collapse any pre-existing duplicates in
	// the same hit. limit=3 was the pre-P1 #7 value and was too small
	// to see redundancy beyond the topmost matches.
	if h.cold != nil && len(m.Embedding) > 0 && h.resolver != nil {
		candidates, err := h.cold.RetrieveByVector(m.Embedding, m.UserID, m.ProjectID, h.DedupOversample())
		if err == nil && len(candidates) > 0 {
			conflicts := h.resolver.FindConflicts(m, candidates)
			if len(conflicts) > 0 {
				if err := h.dedupMerge(ctx, conflicts, m); err != nil {
					return err
				}
				return nil
			}
		}
		// No conflict path: report it so dashboards can distinguish
		// "we never have conflicts because the resolver isn't firing"
		// from "we genuinely don't have similar memories".
		metrics.MemoryConflictTotal.WithLabelValues("none").Inc()
	}

	// New-insert path: cold first (source of truth), then hot (cache).
	if h.cold != nil {
		if err := h.cold.Store(m); err != nil {
			// AUDIT-P2-4 critical: cold is the source of truth, so failing
			// here means the memory is *lost* — no other layer compensates.
			// Emit an Error log + failures_total{severity="critical"} so
			// dashboards can page on `rate(...{severity="critical"}[5m]) > 0`.
			metrics.MemoryFailuresTotal.WithLabelValues("cold", "store", "critical").Inc()
			h.logger.Error("cold store write failed (memory lost, no compensating path)",
				zap.Error(err), zap.String("id", m.ID))
			return err
		}
	}
	if h.hot != nil {
		if err := h.hot.Store(ctx, m); err != nil {
			// AUDIT-P2-4 warn: cold has the truth — cache miss rate signal.
			metrics.MemoryFailuresTotal.WithLabelValues("hot", "store", "warn").Inc()
			h.logger.Warn("hot store write failed (cold succeeded, cache miss only)",
				zap.Error(err), zap.String("id", m.ID))
		}
	}

	h.publishEvent(ctx, "added", m)
	return nil
}

// publishEvent broadcasts a memory mutation. Failures are logged but never
// propagated — the blackboard is opportunistic notification, not a primary
// persistence path.
func (h *HybridStore) publishEvent(ctx context.Context, action string, m *Memory) {
	if h.blackboard == nil {
		return
	}
	if err := h.blackboard.Publish(ctx, action, m); err != nil {
		metrics.MemoryBlackboardPublishTotal.WithLabelValues(action, "err").Inc()
		metrics.MemoryFailuresTotal.WithLabelValues("blackboard", "publish", "warn").Inc()
		h.logger.Warn("blackboard publish failed (subscribers will miss event)",
			zap.String("action", action),
			zap.String("memory_id", m.ID),
			zap.Error(err))
	}
}
