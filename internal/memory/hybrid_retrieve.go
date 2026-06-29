package memory

import (
	"context"
	"time"

	"github.com/agent/code_agent/internal/metrics"
	"go.uber.org/zap"
)

// Retrieve searches memories using a hot+cold *fusion* (Reciprocal Rank
// Fusion, k=60). This replaces the previous "hot wins if it has enough
// items" short-circuit, which silently buried long-tail high-value
// memories (only in cold) whenever the hot layer had noisy recent items.
//
// Strategy:
//  1. Query both layers in parallel (each with its own semantic ranking).
//  2. Combine by RRF: score(d) = Σ 1 / (k + rank_layer(d)).
//  3. Apply per-source bonus — hot results get a small +ε so cache-warm
//     memories beat ties (still cache-friendly behaviour).
//
// Falls back gracefully:
//   - embedder unavailable → cold ILIKE text search only.
//   - cold absent → hot only (previous behaviour, no regression).
//   - hot absent → cold only.
func (h *HybridStore) Retrieve(ctx context.Context, userID, projectID, query string, limit int) (out []Memory, retErr error) {
	start := time.Now()
	tier := "hybrid"
	defer func() {
		status := "ok"
		if retErr != nil {
			status = "err"
		}
		metrics.MemoryRetrieveTotal.WithLabelValues(tier, status).Inc()
		metrics.MemoryRetrieveDuration.WithLabelValues(tier).Observe(time.Since(start).Seconds())
		metrics.MemoryRetrieveResultCount.Observe(float64(len(out)))
		// Record access on the returned set so Decay treats actually-
		// used memories differently from cold ones. Non-blocking; safe
		// on every exit including the degraded paths.
		h.enqueueTouches(out)
	}()

	if limit <= 0 {
		limit = 5
	}
	queryEmbedding := h.embedText(ctx, query)

	// Degraded path: no embedder → cold-only ILIKE search.
	if queryEmbedding == nil {
		h.recordRetrieveEmbedderDegraded(ctx, userID, projectID, query)
		if h.cold != nil {
			tier = "cold"
			return h.cold.Retrieve(userID, projectID, query, limit)
		}
		if h.hot != nil {
			tier = "hot"
			return h.hot.Retrieve(ctx, userID, projectID, limit)
		}
		tier = "none"
		return nil, nil
	}

	// Over-fetch from each layer so RRF has signal beyond the top-`limit`.
	overFetch := limit * 3
	if overFetch < 10 {
		overFetch = 10
	}

	var hotMems, coldMems []Memory
	if h.hot != nil {
		ms, err := h.hot.RetrieveByQuery(ctx, userID, projectID, queryEmbedding, overFetch)
		if err != nil {
			metrics.MemoryFailuresTotal.WithLabelValues("hot", "retrieve", "warn").Inc()
			h.logger.Warn("hot retrieve failed (falling back to cold-only)",
				zap.String("user_id", userID),
				zap.Error(err))
		} else {
			hotMems = ms
		}
	}
	if h.cold != nil {
		ms, err := h.cold.RetrieveByVector(queryEmbedding, userID, projectID, overFetch)
		if err != nil {
			metrics.MemoryFailuresTotal.WithLabelValues("cold", "retrieve", "error").Inc()
			h.logger.Error("cold retrieve failed (PG query error, results degraded)",
				zap.String("user_id", userID),
				zap.Error(err))
		} else {
			coldMems = ms
		}
	}

	fused := fuseRRF(hotMems, coldMems, limit)
	// P1 #8: promote cold-only hits with high score so subsequent
	// retrievals can hit the 5ms hot path. Non-blocking — drops if
	// queue is full.
	h.enqueuePromote(hotMems, fused)
	return fused, nil
}

// dedupCandidateLimitCap is the maximum K accepted by RetrieveCandidates.
// 200 chosen so a worst-case Extractor.isDuplicate call costs at most one
// pgvector IVFFlat scan with k=200 (typically <30ms) plus one Redis
// SCAN+pipeline GET (typically <10ms). Beyond that we'd be reading the
// whole tenant's hot set, which the user-detected duplicate doesn't need.
const dedupCandidateLimitCap = 200

// RetrieveCandidates is the P1 #9 dedup-only read path. It exists to
// give Extractor.isDuplicate a large candidate window (default 30 vs the
// previous 5 from a top-K Retrieve) without the side effects of the
// user-search path:
//
//   - no enqueueTouches: dedup queries must not bump AccessCount or
//     LastAccessedAt — that would lie to Decay about "memory was used".
//   - no enqueuePromote: dedup queries must not warm hot with noise —
//     a 0.4-score cold-only candidate found while we're checking for
//     duplicates is not signal that "this memory is hot".
//   - no RRF fusion: we don't care about presentation ranking, only
//     "is there any candidate within 0.85 cosine?". A union of the two
//     tiers (deduped by Memory.ID) is the cheapest correct answer.
//
// Embedding is required; empty embedding returns nil so the caller can
// fall back to the legacy Retrieve(content, 5) path (only used when no
// Embedder is wired). Limit is clamped to [1, dedupCandidateLimitCap].
//
// Both tiers fail-soft: a Redis or PG error on one side still returns
// whatever the other tier produced. Total failure returns nil with no
// error — dedup is best-effort; an unreachable store should not block
// new Extractor writes (ConflictResolver still cleans up post-Store via
// the P1 #7 anchor+drain kernel).
func (h *HybridStore) RetrieveCandidates(ctx context.Context, userID, projectID string, embedding []float32, limit int) ([]Memory, error) {
	if len(embedding) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > dedupCandidateLimitCap {
		limit = dedupCandidateLimitCap
	}

	var hotMems, coldMems []Memory
	if h.hot != nil {
		if ms, err := h.hot.RetrieveByQuery(ctx, userID, projectID, embedding, limit); err == nil {
			hotMems = ms
		} else {
			metrics.MemoryFailuresTotal.WithLabelValues("hot", "retrieve", "warn").Inc()
			h.logger.Warn("dedup candidate hot lookup failed",
				zap.String("user_id", userID),
				zap.Error(err))
		}
	}
	if h.cold != nil {
		if ms, err := h.cold.RetrieveByVector(embedding, userID, projectID, limit); err == nil {
			coldMems = ms
		} else {
			metrics.MemoryFailuresTotal.WithLabelValues("cold", "retrieve", "error").Inc()
			h.logger.Error("dedup candidate cold lookup failed (PG query error)",
				zap.String("user_id", userID),
				zap.Error(err))
		}
	}

	// Union, dedup by ID. Hot first so the loop's first-seen winner is
	// the freshest copy (hot writes win on conflicting embeddings since
	// hot is closer to the most recent Store + Touch path).
	seen := make(map[string]struct{}, len(hotMems)+len(coldMems))
	out := make([]Memory, 0, len(hotMems)+len(coldMems))
	for _, m := range hotMems {
		if m.ID == "" {
			continue
		}
		if _, dup := seen[m.ID]; dup {
			continue
		}
		seen[m.ID] = struct{}{}
		out = append(out, m)
	}
	for _, m := range coldMems {
		if m.ID == "" {
			continue
		}
		if _, dup := seen[m.ID]; dup {
			continue
		}
		seen[m.ID] = struct{}{}
		out = append(out, m)
	}
	return out, nil
}

// RetrieveByType is the type-filtered semantic search variant of Retrieve.
// Used by orchestrator.buildLongTermMemory's importance bucketing to
// guarantee diversity (≥1 preference + ≥1 decision in the prompt) instead
// of letting one noisy "knowledge" type swamp the top-K.
//
// Cold-tier path: pgvector ORDER BY <=> with WHERE type = ?, cheap because
// (user_id, project_id) index already narrows the candidate set.
// Hot-tier path: full SCAN + in-memory filter; acceptable because the hot
// tier is bounded (≤ 50 entries per user/project, see 25_memory.md::§6).
func (h *HybridStore) RetrieveByType(ctx context.Context, userID, projectID, memType, query string, limit int) (out []Memory, retErr error) {
	start := time.Now()
	tier := "hybrid"
	defer func() {
		status := "ok"
		if retErr != nil {
			status = "err"
		}
		metrics.MemoryRetrieveTotal.WithLabelValues(tier, status).Inc()
		metrics.MemoryRetrieveDuration.WithLabelValues(tier).Observe(time.Since(start).Seconds())
		metrics.MemoryRetrieveResultCount.Observe(float64(len(out)))
		h.enqueueTouches(out)
	}()

	if limit <= 0 {
		limit = 5
	}
	if memType == "" {
		// No type filter requested → semantically identical to Retrieve;
		// dispatch there so we don't duplicate the RRF fusion logic.
		return h.Retrieve(ctx, userID, projectID, query, limit)
	}

	queryEmbedding := h.embedText(ctx, query)

	// Degraded path: no embedder → cold-only fallback (no semantic search).
	if queryEmbedding == nil {
		h.recordRetrieveEmbedderDegraded(ctx, userID, projectID, query)
		if h.cold != nil {
			tier = "cold"
			// PG ILIKE doesn't filter by type — do client-side filter.
			all, err := h.cold.Retrieve(userID, projectID, query, limit*4)
			if err != nil {
				return nil, err
			}
			return filterByType(all, memType, limit), nil
		}
		tier = "none"
		return nil, nil
	}

	overFetch := limit * 3
	if overFetch < 10 {
		overFetch = 10
	}

	var hotMems, coldMems []Memory
	if h.hot != nil {
		// Hot has no type-aware index; we over-fetch then filter client-side.
		// Acceptable because hot is small (≤ 50 entries).
		all, err := h.hot.RetrieveByQuery(ctx, userID, projectID, queryEmbedding, overFetch*2)
		if err != nil {
			metrics.MemoryFailuresTotal.WithLabelValues("hot", "retrieve", "warn").Inc()
			h.logger.Warn("hot retrieve by type failed",
				zap.String("user_id", userID),
				zap.Error(err))
		} else {
			hotMems = filterByType(all, memType, overFetch)
		}
	}
	if h.cold != nil {
		ms, err := h.cold.RetrieveByVectorAndType(queryEmbedding, userID, projectID, memType, overFetch)
		if err != nil {
			metrics.MemoryFailuresTotal.WithLabelValues("cold", "retrieve", "error").Inc()
			h.logger.Error("cold retrieve by type failed (PG query error)",
				zap.String("user_id", userID),
				zap.Error(err))
		} else {
			coldMems = ms
		}
	}

	fused := fuseRRF(hotMems, coldMems, limit)
	h.enqueuePromote(hotMems, fused)
	return fused, nil
}

// filterByType keeps only memories whose Type matches `t`, up to `limit`.
func filterByType(in []Memory, t string, limit int) []Memory {
	out := make([]Memory, 0, len(in))
	for _, m := range in {
		if string(m.Type) != t {
			continue
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out
}
