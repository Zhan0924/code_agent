package memory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/agent/code_agent/internal/metrics"
	"go.uber.org/zap"
)

// dedupOversample is the candidate budget HybridStore.Store gives the
// vector search when looking for conflicts. The pre-P1 #7 value was 3,
// which was insufficient to catch duplicate backlog (the duplicates
// past rank-3 stayed alive forever). 10 is conservative — small enough
// that pgvector still hits the IVFFlat index efficiently, large enough
// that mid-size duplicate clusters get cleaned up in one Store call.
//
// Capped further by ConflictResolver.MaxConflicts() before we touch
// cold transactions — see HybridStore.dedupMerge.
const dedupOversample = 10

// Embedder generates vector embeddings from text.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// HybridStore combines Redis (hot) and PostgreSQL (cold) memory stores.
// Reads fuse hot + cold via RRF; writes go to both with best-effort
// consistency + outbox-style event publishing.
type HybridStore struct {
	hot        *RedisHot
	cold       *PGCold
	embedder   Embedder
	logger     *zap.Logger
	blackboard *Blackboard
	resolver   *ConflictResolver

	// touchQueue + accessOpts implement the P0 #4 read-path
	// access_count / last_accessed_at batcher. nil queue means
	// "feature disabled" — enqueueTouches becomes a no-op.
	//
	// P0 #5: the queue carries full TouchRef (not just ID) so the
	// batcher can refresh hot's JSON in place — hot's Redis key is
	// `memory:<userID>:<projectID>:<id>` and the prefix is required
	// to reconstruct it.
	touchQueue chan TouchRef
	accessOpts AccessBatcherOptions

	// promoteQueue + promoteOpts implement the P1 #8 read-path
	// cold→hot back-fill. nil queue means "feature disabled" —
	// enqueuePromote becomes a no-op. Channel carries full Memory
	// values (not just IDs) because RedisHot.PromoteBatch needs the
	// JSON to SET.
	promoteQueue chan Memory
	promoteOpts  PromoteOptions

	// demoteThreshold is the P1 #8 hot-eviction floor used by Decay.
	// Zero (the default) disables demotion — Decay keeps the
	// pre-fix behavior (SET with reduced score, KeepTTL). Set via
	// SetDemoteThreshold from main.go.
	demoteThreshold float64

	masker *PIIMasker
}

// NewHybridStore creates a hybrid memory store.
func NewHybridStore(hot *RedisHot, cold *PGCold, logger *zap.Logger) *HybridStore {
	return &HybridStore{
		hot:      hot,
		cold:     cold,
		logger:   logger.With(zap.String("component", "memory.hybrid")),
		resolver: NewConflictResolver(cold),
		masker:   NewPIIMasker(),
	}
}

// SetBlackboard sets the blackboard for publishing events.
func (h *HybridStore) SetBlackboard(b *Blackboard) {
	h.blackboard = b
}

// SetEmbedder injects an embedder for semantic operations.
func (h *HybridStore) SetEmbedder(e Embedder) {
	h.embedder = e
}

// SetConflictResolver allows callers (main.go) to swap in a custom
// resolver — e.g. one with project-specific thresholds.
func (h *HybridStore) SetConflictResolver(r *ConflictResolver) {
	if r != nil {
		h.resolver = r
	}
}

// embedText generates an embedding for a single text, returning nil on failure.
func (h *HybridStore) embedText(ctx context.Context, text string) []float32 {
	if h.embedder == nil {
		metrics.MemoryFailuresTotal.WithLabelValues("embedder", "embed", "warn").Inc()
		h.logger.Warn("embedder is nil, retrieval will degrade to ILIKE",
			zap.String("audit_id", "REAUDIT-P0-4"),
			zap.String("op", "embedder_unavailable"),
			zap.String("result", "degraded"))
		return nil
	}
	vecs, err := h.embedder.Embed(ctx, []string{text})
	if err != nil || len(vecs) == 0 {
		metrics.MemoryFailuresTotal.WithLabelValues("embedder", "embed", "error").Inc()
		h.logger.Warn("embedding failed (retrieval quality degraded)",
			zap.String("audit_id", "REAUDIT-P0-4"),
			zap.String("op", "embed_failed"),
			zap.Error(err),
			zap.String("result", "degraded"))
		return nil
	}
	return vecs[0]
}

// recordRetrieveEmbedderDegraded emits REAUDIT-P0-4 observability when
// semantic retrieve falls back to ILIKE because embedText returned nil.
func (h *HybridStore) recordRetrieveEmbedderDegraded(ctx context.Context, userID, projectID, query string) {
	reason := "embedder_failed"
	if h.embedder == nil {
		reason = "embedder_nil"
	}
	metrics.MemoryRetrieveDegradedTotal.WithLabelValues(reason).Inc()
	h.logger.Warn("retrieve degraded to ILIKE text search",
		zap.String("audit_id", "REAUDIT-P0-4"),
		zap.String("op", "retrieve_degraded"),
		zap.String("reason", reason),
		zap.String("user_id", userID),
		zap.String("project_id", projectID),
		zap.String("query", query),
		zap.String("result", "degraded"))
}

// RetrieveWithEmbedder runs Retrieve using a temporary embedder override.
// Dev-only smoke tests use this to exercise embedder failure paths without
// mutating the production embedder wired at startup.
func (h *HybridStore) RetrieveWithEmbedder(ctx context.Context, emb Embedder, userID, projectID, query string, limit int) ([]Memory, error) {
	prev := h.embedder
	h.embedder = emb
	defer func() { h.embedder = prev }()
	return h.Retrieve(ctx, userID, projectID, query, limit)
}

// TestFailingEmbedder is a dev-only embedder that always errors. Used by
// verify-reaudit-p0-4.sh to exercise degrade observability without
// mutating the production embedder wired at startup.
type TestFailingEmbedder struct{}

func (TestFailingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, fmt.Errorf("injected test embedder failure")
}

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
		candidates, err := h.cold.RetrieveByVector(m.Embedding, m.UserID, m.ProjectID, dedupOversample)
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

// fuseRRF applies Reciprocal Rank Fusion to two ranked lists.
// k=60 is the canonical value from the original RRF paper (Cormack et al.).
const rrfK = 60.0

// hotBonus is added to hot-list scores so cache-warm items break ties.
// Magnitude chosen so a hot-list rank-5 still beats a cold-list rank-1 in
// pure isolation, encouraging recently-discussed context to surface.
const hotBonus = 1.0 / (rrfK + 0)

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
