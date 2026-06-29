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
}

// PromoteOptions tunes the read-path cold→hot back-fill batcher.
// The defaults are sized for a Retrieve QPS of ~50/s with ~20%
// cold-only-hit rate: 50 * 0.2 = 10 promotes/s ≪ BatchSize 50
// and well under QueueSize 256.
type PromoteOptions struct {
	// Threshold: only promote memories with Score >= Threshold. Score
	// is the importance signal from Extractor / ConflictResolver, so
	// 0.7 keeps tier-1 cached without polluting hot with noise.
	Threshold float64
	// BatchSize: single-pipeline SET fan-out cap.
	BatchSize int
	// FlushInterval: timer-based flush for partial batches.
	FlushInterval time.Duration
	// QueueSize: bounded chan; overflow → drop + metric.
	QueueSize int
}

func (o PromoteOptions) withDefaults() PromoteOptions {
	if o.Threshold <= 0 {
		o.Threshold = 0.7
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 50
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 5 * time.Second
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 256
	}
	return o
}

// AccessBatcherOptions tunes the read-path Touch debouncer. The defaults
// (`.withDefaults()`) are calibrated for "200 QPS Retrieve × 5 results
// stable state" — i.e. ~1000 IDs/s of throughput.
type AccessBatcherOptions struct {
	// BatchSize caps the number of IDs per UPDATE round-trip. Default 100.
	BatchSize int
	// FlushInterval forces a flush even when BatchSize isn't reached.
	// Default 5s — bounds Decay's last_accessed_at staleness.
	FlushInterval time.Duration
	// QueueSize bounds the in-memory chan capacity. Default 1024.
	// At saturation the read path drops IDs (non-blocking) and metrics
	// surface the back-pressure via MemoryTouchQueueDropsTotal.
	QueueSize int
}

func (o AccessBatcherOptions) withDefaults() AccessBatcherOptions {
	if o.BatchSize <= 0 {
		o.BatchSize = 100
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 5 * time.Second
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 1024
	}
	return o
}

// NewHybridStore creates a hybrid memory store.
func NewHybridStore(hot *RedisHot, cold *PGCold, logger *zap.Logger) *HybridStore {
	return &HybridStore{
		hot:      hot,
		cold:     cold,
		logger:   logger.With(zap.String("component", "memory.hybrid")),
		resolver: NewConflictResolver(cold),
	}
}

// EnableAccessBatcher allocates the touchQueue and stores the tuning
// options. Call this before StartAccessBatcher; the read path checks
// for a non-nil touchQueue to decide whether to enqueue. main.go
// invokes this once during construction with config-driven options.
func (h *HybridStore) EnableAccessBatcher(opts AccessBatcherOptions) {
	h.accessOpts = opts.withDefaults()
	h.touchQueue = make(chan TouchRef, h.accessOpts.QueueSize)
}

// EnablePromoteBatcher allocates the promoteQueue and stores the
// tuning options. Pairs with StartPromoteBatcher (background goroutine)
// and enqueuePromote (read-path hook). All three must run for the
// P1 #8 cold→hot back-fill to take effect.
func (h *HybridStore) EnablePromoteBatcher(opts PromoteOptions) {
	h.promoteOpts = opts.withDefaults()
	h.promoteQueue = make(chan Memory, h.promoteOpts.QueueSize)
}

// SetDemoteThreshold configures the P1 #8 hot-eviction floor used by
// the Decay path. Zero (default) keeps the legacy behavior of "SET with
// reduced score" — no DELs from hot during decay. Positive value
// triggers DEL when an entry's score crosses below the threshold this
// iteration. Must be > 0.01 to interact meaningfully with the existing
// score floor.
func (h *HybridStore) SetDemoteThreshold(t float64) {
	h.demoteThreshold = t
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
			// Critical: do NOT write hot if cold failed. Otherwise hot has
			// a record that will silently disappear at TTL expiry.
			return err
		}
	}
	if h.hot != nil {
		if err := h.hot.Store(ctx, m); err != nil {
			// Non-fatal — cold has the truth. Note the asymmetry vs the
			// previous code which logged at Debug; we bump to Warn so this
			// shows up in production dashboards (cache miss rate signal).
			h.logger.Warn("hot store write failed", zap.Error(err),
				zap.String("id", m.ID))
		}
	}

	h.publishEvent(ctx, "added", m)
	return nil
}

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

// publishEvent broadcasts a memory mutation. Failures are logged but never
// propagated — the blackboard is opportunistic notification, not a primary
// persistence path.
func (h *HybridStore) publishEvent(ctx context.Context, action string, m *Memory) {
	if h.blackboard == nil {
		return
	}
	if err := h.blackboard.Publish(ctx, action, m); err != nil {
		h.logger.Debug("blackboard publish failed",
			zap.String("action", action), zap.Error(err))
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
			h.logger.Debug("hot retrieve failed", zap.Error(err))
		} else {
			hotMems = ms
		}
	}
	if h.cold != nil {
		ms, err := h.cold.RetrieveByVector(queryEmbedding, userID, projectID, overFetch)
		if err != nil {
			h.logger.Debug("cold retrieve failed", zap.Error(err))
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
			h.logger.Debug("dedup candidate hot lookup failed", zap.Error(err))
		}
	}
	if h.cold != nil {
		if ms, err := h.cold.RetrieveByVector(embedding, userID, projectID, limit); err == nil {
			coldMems = ms
		} else {
			h.logger.Debug("dedup candidate cold lookup failed", zap.Error(err))
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

func fuseRRF(hot, cold []Memory, limit int) []Memory {
	type entry struct {
		mem   Memory
		score float64
	}
	merged := make(map[string]*entry, len(hot)+len(cold))

	for i, m := range hot {
		e, ok := merged[m.ID]
		if !ok {
			e = &entry{mem: m}
			merged[m.ID] = e
		}
		e.score += 1.0/(rrfK+float64(i+1)) + hotBonus
	}
	for i, m := range cold {
		e, ok := merged[m.ID]
		if !ok {
			e = &entry{mem: m}
			merged[m.ID] = e
		}
		e.score += 1.0 / (rrfK + float64(i+1))
	}

	out := make([]entry, 0, len(merged))
	for _, e := range merged {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })

	if len(out) > limit {
		out = out[:limit]
	}
	result := make([]Memory, len(out))
	for i, e := range out {
		result[i] = e.mem
	}
	return result
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
			h.logger.Debug("hot retrieve failed", zap.Error(err))
		} else {
			hotMems = filterByType(all, memType, overFetch)
		}
	}
	if h.cold != nil {
		ms, err := h.cold.RetrieveByVectorAndType(queryEmbedding, userID, projectID, memType, overFetch)
		if err != nil {
			h.logger.Debug("cold retrieve failed", zap.Error(err))
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

// enqueueTouches is the read-path hook: after a retrieval returns N
// memories, we want to record "these were accessed" without paying N
// UPDATEs of latency. The batcher goroutine (StartAccessBatcher) folds
// many enqueues into a single UPDATE round-trip — and into a single
// hot pipeline GET+SET, since P0 #5.
//
// Non-blocking by design: if the queue is full, we drop and emit a
// metric. Better to under-count accesses than to slow down reads.
func (h *HybridStore) enqueueTouches(ms []Memory) {
	if h.touchQueue == nil {
		return
	}
	for _, m := range ms {
		if m.ID == "" {
			continue
		}
		ref := TouchRef{UserID: m.UserID, ProjectID: m.ProjectID, ID: m.ID}
		select {
		case h.touchQueue <- ref:
		default:
			metrics.MemoryTouchQueueDropsTotal.Inc()
		}
	}
}

// flushTouches is the batcher's terminal step: dual-write cold + hot.
// Cold is the source of truth — its error decides status="ok"|"err".
// Hot is best-effort: a stale hot copy just makes the next Retrieve
// see slightly older AccessCount/LastAccessedAt, but cold has the
// truth and Decay reads from cold.
func (h *HybridStore) flushTouches(refs []TouchRef) error {
	if len(refs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var coldErr error
	if h.cold != nil {
		ids := make([]string, len(refs))
		for i, r := range refs {
			ids[i] = r.ID
		}
		coldErr = h.cold.TouchBatch(ctx, ids)
	}
	if h.hot != nil {
		if err := h.hot.TouchBatch(ctx, refs); err != nil {
			// Hot drift is recoverable — cold is the durable record.
			// Surface for ops awareness but don't fail the batch.
			h.logger.Warn("hot touch batch failed (cold remains source of truth)",
				zap.Int("refs", len(refs)), zap.Error(err))
		}
	}
	return coldErr
}

// StartAccessBatcher runs the debouncing flusher. Returns when ctx is
// cancelled, after draining any pending refs in one last flush.
//
// Concurrency contract: caller invokes this exactly once in a goroutine
// after EnableAccessBatcher. Multiple batchers would race on the same
// touchQueue and produce non-deterministic flush sizes; we deliberately
// don't guard against that — main.go is the sole caller.
func (h *HybridStore) StartAccessBatcher(ctx context.Context) {
	if h.touchQueue == nil {
		return
	}
	if h.cold == nil && h.hot == nil {
		return
	}
	h.logger.Info("memory access batcher started",
		zap.Int("batch_size", h.accessOpts.BatchSize),
		zap.Duration("flush_interval", h.accessOpts.FlushInterval),
		zap.Int("queue_size", h.accessOpts.QueueSize))
	defer h.logger.Info("memory access batcher stopping")

	runAccessBatcherLoop(ctx, h.touchQueue, h.accessOpts.BatchSize, h.accessOpts.FlushInterval, h.flushTouches, h.logger)
}

// runAccessBatcherLoop is the pure state machine of the access batcher.
// Exposed unexported for testing — see TestRunAccessBatcherLoop_*.
//
// flush is invoked on every drained batch with a slice the loop owns;
// the callee MUST NOT retain the slice past the call. Status metrics
// are recorded here so the same observability holds regardless of how
// HybridStore wires up the actual UPDATE.
//
// Dedup key is TouchRef.ID — within a single batch window we only need
// one update per memory regardless of how many reads referenced it.
// (user, project, id) is technically the more precise key, but IDs are
// already globally unique UUIDs so collisions across tenants are zero.
func runAccessBatcherLoop(
	ctx context.Context,
	queue <-chan TouchRef,
	batchSize int,
	flushInterval time.Duration,
	flush func([]TouchRef) error,
	logger *zap.Logger,
) {
	timer := time.NewTimer(flushInterval)
	defer timer.Stop()
	buf := make([]TouchRef, 0, batchSize)
	seen := make(map[string]struct{}, batchSize)

	doFlush := func() {
		if len(buf) == 0 {
			return
		}
		err := flush(buf)
		status := "ok"
		if err != nil {
			status = "err"
			if logger != nil {
				logger.Warn("touch batch flush failed", zap.Int("ids", len(buf)), zap.Error(err))
			}
		}
		metrics.MemoryTouchBatchTotal.WithLabelValues(status).Inc()
		metrics.MemoryTouchBatchSize.Observe(float64(len(buf)))
		buf = buf[:0]
		for k := range seen {
			delete(seen, k)
		}
	}

	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(flushInterval)
	}

	for {
		select {
		case <-ctx.Done():
			doFlush()
			return
		case ref, ok := <-queue:
			if !ok {
				doFlush()
				return
			}
			if _, dup := seen[ref.ID]; dup {
				continue
			}
			seen[ref.ID] = struct{}{}
			buf = append(buf, ref)
			if len(buf) >= batchSize {
				doFlush()
				resetTimer()
			}
		case <-timer.C:
			doFlush()
			timer.Reset(flushInterval)
		}
	}
}

// StartPromoteBatcher runs the cold→hot back-fill flusher. Returns
// when ctx is cancelled, after draining any pending memories in one
// last flush. Mirrors StartAccessBatcher's contract — one caller
// (main.go) invokes this once in a goroutine after
// EnablePromoteBatcher.
func (h *HybridStore) StartPromoteBatcher(ctx context.Context) {
	if h.promoteQueue == nil || h.hot == nil {
		return
	}
	h.logger.Info("memory promote batcher started",
		zap.Float64("threshold", h.promoteOpts.Threshold),
		zap.Int("batch_size", h.promoteOpts.BatchSize),
		zap.Duration("flush_interval", h.promoteOpts.FlushInterval),
		zap.Int("queue_size", h.promoteOpts.QueueSize))
	defer h.logger.Info("memory promote batcher stopping")

	runPromoteBatcherLoop(ctx, h.promoteQueue, h.promoteOpts.BatchSize, h.promoteOpts.FlushInterval, h.flushPromotes, h.logger)
}

// enqueuePromote scans the fused retrieval result for "cold-only hits
// with score >= threshold" — those are the entries we want cached in
// hot so subsequent retrievals hit the 5ms path. hot-already-cached
// items are skipped (no point SET-ing what's already there); the
// threshold check filters out low-signal entries that don't justify
// the hot tier's 24h footprint.
//
// Non-blocking by design (read latency must not couple to promote
// latency). Queue overflow → drop + metric. Nil queue → no-op.
func (h *HybridStore) enqueuePromote(hot, fused []Memory) {
	if h.promoteQueue == nil || h.hot == nil {
		return
	}
	hotIDs := make(map[string]struct{}, len(hot))
	for _, m := range hot {
		hotIDs[m.ID] = struct{}{}
	}
	for _, m := range fused {
		if m.ID == "" {
			continue
		}
		if _, already := hotIDs[m.ID]; already {
			continue
		}
		if m.Score < h.promoteOpts.Threshold {
			continue
		}
		select {
		case h.promoteQueue <- m:
		default:
			metrics.MemoryPromoteQueueDropsTotal.Inc()
		}
	}
}

// flushPromotes is the batcher's terminal step. Delegates to
// hot.PromoteBatch which does the actual pipeline SET. Cold isn't
// touched here — promote is a hot-only operation by definition.
func (h *HybridStore) flushPromotes(mems []Memory) error {
	if h.hot == nil || len(mems) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.hot.PromoteBatch(ctx, mems)
}

// runPromoteBatcherLoop is the pure state machine of the promote
// batcher. Exposed unexported for testing — see TestRunPromoteBatcher*.
// Dedup key is Memory.ID (same as access batcher).
func runPromoteBatcherLoop(
	ctx context.Context,
	queue <-chan Memory,
	batchSize int,
	flushInterval time.Duration,
	flush func([]Memory) error,
	logger *zap.Logger,
) {
	timer := time.NewTimer(flushInterval)
	defer timer.Stop()
	buf := make([]Memory, 0, batchSize)
	seen := make(map[string]struct{}, batchSize)

	doFlush := func() {
		if len(buf) == 0 {
			return
		}
		err := flush(buf)
		status := "ok"
		if err != nil {
			status = "err"
			if logger != nil {
				logger.Warn("promote batch flush failed", zap.Int("items", len(buf)), zap.Error(err))
			}
		}
		metrics.MemoryPromoteTotal.WithLabelValues(status).Inc()
		metrics.MemoryPromoteBatchSize.Observe(float64(len(buf)))
		buf = buf[:0]
		for k := range seen {
			delete(seen, k)
		}
	}

	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(flushInterval)
	}

	for {
		select {
		case <-ctx.Done():
			doFlush()
			return
		case m, ok := <-queue:
			if !ok {
				doFlush()
				return
			}
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			buf = append(buf, m)
			if len(buf) >= batchSize {
				doFlush()
				resetTimer()
			}
		case <-timer.C:
			doFlush()
			timer.Reset(flushInterval)
		}
	}
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
