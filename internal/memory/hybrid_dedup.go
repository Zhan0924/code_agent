package memory

import (
	"context"
	"sort"

	"github.com/agent/code_agent/internal/metrics"
	"go.uber.org/zap"
)

// dedupMerge is the P1 #7 conflict-resolution kernel. Given the
// non-empty list of conflicting memories we found in cold + the new
// memory being stored, it:
//
//  1. Picks one anchor (highest score, then access count, then oldest
//     by CreatedAt — see PickAnchor for the full tie-breaker order).
//  2. Reinforces the anchor's AccessCount + LastAccessedAt with every
//     non-anchor duplicate's signal — so the surviving entry inherits
//     the cumulative "this concept was seen N times" weight.
//  3. Folds the new memory into the anchor via ResolveWithOutcome
//     (which decides override / preserve / merge based on score gap).
//  4. Writes the merged anchor + deletes the non-anchor IDs in a single
//     cold transaction (PGCold.DedupTx). Either both land or nothing
//     changes — no half-state where dups stay alive after anchor lost.
//  5. Best-effort: drops the same IDs from hot via DeleteBatch and
//     re-publishes the anchor to hot via Store.
//  6. Emits metrics — outcome="dedup" for the conflict counter, plus
//     dedup_removed_total (count of deletions) and dedup_batch_size
//     histogram for percentile tracking.
//
// Returns nil on success or the underlying cold-transaction error so
// the caller (Store) can surface it to the metrics decorator. If the
// dedup branch was a no-op (e.g. only one conflict and the anchor IS
// that one — so dupIDs is empty), we still go through DedupTx so the
// reinforcement on the anchor is committed.
func (h *HybridStore) dedupMerge(ctx context.Context, conflicts []Memory, newMem *Memory) error {
	if len(conflicts) == 0 {
		return nil
	}

	// Cap to MaxConflicts: a runaway candidate set (e.g. 200 highly
	// similar entries) shouldn't tie up a multi-second DELETE.
	// We process the top-N by anchor priority — PickAnchor already
	// sorts implicitly via its scan, but we materialise the order
	// here so the cap is applied to the *most relevant* conflicts.
	maxN := h.resolver.MaxConflicts()
	if len(conflicts) > maxN {
		// Sort conflicts by anchor priority DESC so the cap keeps the
		// "best" candidates — same comparator as PickAnchor.
		sort.SliceStable(conflicts, func(i, j int) bool {
			return anchorBeats(conflicts[i], conflicts[j])
		})
		conflicts = conflicts[:maxN]
	}

	anchorIdx := PickAnchor(conflicts)
	anchor := conflicts[anchorIdx]

	dupIDs := make([]string, 0, len(conflicts)-1)
	dupRefs := make([]TouchRef, 0, len(conflicts)-1)
	for i, c := range conflicts {
		if i == anchorIdx {
			continue
		}
		// Fold the duplicate's reinforcement counters into the anchor
		// before we drop it. The anchor's content/embedding/score are
		// authoritative — only AccessCount + LastAccessedAt accumulate.
		h.resolver.ReinforceFromDup(&anchor, c)
		dupIDs = append(dupIDs, c.ID)
		dupRefs = append(dupRefs, TouchRef{UserID: c.UserID, ProjectID: c.ProjectID, ID: c.ID})
	}

	// Now resolve the new memory against the (already reinforced)
	// anchor. This is where score-aware override/preserve/merge runs.
	resolved, outcome := h.resolver.ResolveWithOutcome(&anchor, newMem)

	// Cold transaction: anchor UPDATE + dup DELETEs atomically.
	// dupIDs may be empty if len(conflicts)==1; DedupTx tolerates
	// that and just runs the UPDATE.
	if err := h.cold.DedupTx(ctx, resolved, dupIDs); err != nil {
		h.logger.Warn("dedup cold transaction failed",
			zap.Error(err),
			zap.String("anchor_id", resolved.ID),
			zap.Int("dup_count", len(dupIDs)))
		return err
	}

	// Metrics: dedup outcome supersedes the inner ResolveWithOutcome
	// outcome when we actually removed duplicates — operators care
	// about "we cleaned up duplicates" more than "we did a merge-blend
	// on the anchor" in that branch. When dupIDs is empty (single
	// conflict, anchor==conflicts[0]) we fall back to the per-resolve
	// outcome string.
	if len(dupIDs) > 0 {
		metrics.MemoryConflictTotal.WithLabelValues("dedup").Inc()
		metrics.MemoryDedupRemovedTotal.Add(float64(len(dupIDs)))
		metrics.MemoryDedupBatchSize.Observe(float64(len(dupIDs)))
	} else {
		metrics.MemoryConflictTotal.WithLabelValues(string(outcome)).Inc()
	}

	// Hot: best-effort dual write. Drop dup keys then re-store the
	// anchor. Errors are logged at Debug — cold is the source of truth.
	if h.hot != nil {
		if len(dupRefs) > 0 {
			if hotErr := h.hot.DeleteBatch(ctx, dupRefs); hotErr != nil {
				h.logger.Debug("hot dedup DeleteBatch failed",
					zap.Error(hotErr), zap.Int("count", len(dupRefs)))
			}
		}
		if hotErr := h.hot.Store(ctx, resolved); hotErr != nil {
			h.logger.Debug("hot dedup anchor Store failed",
				zap.Error(hotErr), zap.String("id", resolved.ID))
		}
	}

	h.publishEvent(ctx, "merged", resolved)
	return nil
}
