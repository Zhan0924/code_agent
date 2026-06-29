package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/agent/code_agent/internal/metrics"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RedisHot stores recent memories (last 24h) in Redis with TTL auto-expiry.
type RedisHot struct {
	client    *redis.Client
	ttl       time.Duration
	scanLimit int // P1 #10: operator-tunable SCAN ceiling per call.
	logger    *zap.Logger
}

// NewRedisHot creates a hot memory store backed by Redis with the default
// 24h TTL.
func NewRedisHot(client *redis.Client, logger *zap.Logger) *RedisHot {
	return NewRedisHotWithTTL(client, logger, 24*time.Hour)
}

// NewRedisHotWithTTL allows overriding the per-entry TTL. Zero or negative
// values fall back to the 24h default (Redis SET with TTL=0 would mean
// "no TTL" → unbounded growth, which we never want for the hot tier).
func NewRedisHotWithTTL(client *redis.Client, logger *zap.Logger, ttl time.Duration) *RedisHot {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &RedisHot{
		client:    client,
		ttl:       ttl,
		scanLimit: defaultHotScanLimit,
		logger:    logger.With(zap.String("component", "memory.redis_hot")),
	}
}

// SetScanLimit overrides the default per-call SCAN ceiling (P1 #10).
// Values are clamped to [minHotScanLimit, maxHotScanLimit]:
//
//   - <50 would force premature truncation on any tenant with even
//     moderate hot churn; 50 matches the documented per-tenant invariant
//     and keeps the floor recognisable to operators.
//   - >2000 starts to dominate retrieve latency: each SCAN batch of 100
//     keys is one RTT, and the subsequent pipeline GET is O(N). 2000
//     ≈ 20 round-trips + 20k unmarshal cycles ≈ ~30ms worst case.
//
// Pass 0 to retain the default (defaultHotScanLimit). Use this to dial
// up the ceiling for a deployment that legitimately holds thousands of
// hot entries per tenant (e.g. a long-running team workspace) without
// shipping a code change.
func (r *RedisHot) SetScanLimit(n int) {
	switch {
	case n <= 0:
		r.scanLimit = defaultHotScanLimit
	case n < minHotScanLimit:
		r.scanLimit = minHotScanLimit
	case n > maxHotScanLimit:
		r.scanLimit = maxHotScanLimit
	default:
		r.scanLimit = n
	}
}

// ScanLimit returns the currently-configured SCAN ceiling. Exposed for
// tests and for HybridStore-level diagnostics that want to size their
// over-fetch against the hot tier's known budget.
func (r *RedisHot) ScanLimit() int { return r.scanLimit }

func (r *RedisHot) keyPrefix(userID, projectID string) string {
	return fmt.Sprintf("memory:%s:%s", userID, projectID)
}

// Store saves a memory to Redis with TTL.
func (r *RedisHot) Store(ctx context.Context, m *Memory) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s:%s", r.keyPrefix(m.UserID, m.ProjectID), m.ID)
	return r.client.Set(ctx, key, data, r.ttl).Err()
}

// BoostScoreBatch increases the score of specified memories in Redis.
func (r *RedisHot) BoostScoreBatch(ctx context.Context, refs []TouchRef, boost float64) error {
	if len(refs) == 0 {
		return nil
	}
	
	// A lua script to safely get, decode, increment score, and encode back
	script := redis.NewScript(`
		for i, key in ipairs(KEYS) do
			local data = redis.call("GET", key)
			if data then
				local mem = cjson.decode(data)
				if mem.score then
					mem.score = math.min(mem.score + tonumber(ARGV[1]), 1.0)
					redis.call("SET", key, cjson.encode(mem))
				end
			end
		end
		return 1
	`)
	
	keys := make([]string, len(refs))
	for i, ref := range refs {
		keys[i] = r.keyPrefix(ref.UserID, ref.ProjectID) + ":" + ref.ID
	}
	
	return script.Run(ctx, r.client, keys, boost).Err()
}

// TouchBatch refreshes hot copies of each ref: bumps AccessCount and
// rewrites LastAccessedAt to "now". Run in two pipelines — GET all, then
// SET only the ones that actually exist (TTL preserved with KeepTTL so
// touched-but-already-aging keys don't get a free 24h extension).
//
// Race window: between the GET pipeline and the SET pipeline (~ms) a
// concurrent Store() may overwrite the key. We then SET back with the
// older content + bumped counters, losing the Store's content update.
// Mitigations:
//  1. Cold remains the source of truth — the lost write is hot-only.
//  2. ConflictResolver on the next interaction re-derives content.
//  3. Hot's 24h TTL bounds the staleness window anyway.
// We accept the rare race rather than pay for WATCH/MULTI/Lua.
//
// Missing keys (not in hot) are silently skipped — there's no "promote
// from cold" semantics here; Touch is "refresh if cached", not "cache".
func (r *RedisHot) TouchBatch(ctx context.Context, refs []TouchRef) error {
	if len(refs) == 0 {
		return nil
	}
	keys := make([]string, len(refs))
	for i, ref := range refs {
		keys[i] = fmt.Sprintf("%s:%s", r.keyPrefix(ref.UserID, ref.ProjectID), ref.ID)
	}

	getPipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = getPipe.Get(ctx, key)
	}
	_, _ = getPipe.Exec(ctx) // partial errors expected for not-in-hot keys

	setPipe := r.client.Pipeline()
	now := time.Now()
	bumped := 0
	for i, cmd := range cmds {
		data, err := cmd.Result()
		if err != nil {
			continue
		}
		var m Memory
		if json.Unmarshal([]byte(data), &m) != nil {
			continue
		}
		m.AccessCount++
		m.LastAccessedAt = now
		b, err := json.Marshal(&m)
		if err != nil {
			continue
		}
		setPipe.Set(ctx, keys[i], b, redis.KeepTTL)
		bumped++
	}
	if bumped == 0 {
		return nil
	}
	_, err := setPipe.Exec(ctx)
	return err
}

// Hot-tier SCAN ceilings (P1 #10).
//
//   - defaultHotScanLimit: the per-instance default. Matches the pre-fix
//     value so deployments that don't tune anything see no behaviour
//     change. The documented invariant is ≤ 50 entries per (user,
//     project) under the 24h TTL, so 200 gives 4x headroom.
//   - minHotScanLimit / maxHotScanLimit: clamp bounds applied by
//     SetScanLimit. The floor matches the invariant; the ceiling is a
//     safety against pathological configs that would push a single
//     RetrieveByQuery into 100+ms territory.
const (
	defaultHotScanLimit = 200
	minHotScanLimit     = 50
	maxHotScanLimit     = 2000
)

// hotScanLimit is kept as a legacy alias for read paths that still
// expect a static budget (mostly tests). Prefer r.scanLimit on RedisHot
// instances; this is the seed value, not a hard cap.
const hotScanLimit = defaultHotScanLimit

// Retrieve returns recent memories for a user/project from Redis.
//
// Sorted by LastAccessedAt DESC. The prior implementation called
// sort.Strings on raw Redis keys assuming they had a timestamp prefix —
// they didn't (UUIDv4 has no temporal ordering), so the "take most
// recent" semantic was actually "take random subset". This now uses the
// actual Memory.LastAccessedAt after unmarshal.
//
// Episodic memories are filtered out: this method is the fallback for
// degraded recall (no query embedding) and the List path; in both cases
// episodic entries would be noise (they're Distiller fuel, not actionable
// long-term knowledge). The Distiller itself talks directly to PGCold's
// ListEpisodicUndistilled.
//
// Performance: hot tier holds ≤ 50 entries per (user, project) under
// the 24h TTL invariant, so the "Get everything then sort" approach
// costs one pipeline RTT + ~50 unmarshals (sub-ms). At higher fan-out
// MemoryHotScanKeys metric makes the regression visible on dashboards.
func (r *RedisHot) Retrieve(ctx context.Context, userID, projectID string, limit int) ([]Memory, error) {
	pattern := r.keyPrefix(userID, projectID) + ":*"

	// Budget hint = 2x caller's limit so we have headroom after the
	// LastAccessedAt sort + episodic filter. Falls back to scanLimit
	// when the caller asks for "all" (limit<=0).
	budget := 0
	if limit > 0 {
		budget = limit * 2
	}
	keys, err := r.scanAll(ctx, pattern, budget, "retrieve")
	if err != nil {
		return nil, err
	}
	metrics.MemoryHotScanKeys.WithLabelValues("retrieve").Observe(float64(len(keys)))
	if len(keys) == 0 {
		return nil, nil
	}

	pipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.Get(ctx, key)
	}
	_, _ = pipe.Exec(ctx)

	candidates := make([]Memory, 0, len(cmds))
	for _, cmd := range cmds {
		data, err := cmd.Result()
		if err != nil {
			continue
		}
		var m Memory
		if json.Unmarshal([]byte(data), &m) != nil {
			continue
		}
		if m.Type == MemoryTypeEpisodic {
			continue
		}
		candidates = append(candidates, m)
	}

	// True LastAccessedAt-descending sort. Equal-timestamp ties fall
	// through to ID order so the result is deterministic across calls
	// (helps debugging and snapshot-style tests).
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].LastAccessedAt.Equal(candidates[j].LastAccessedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].LastAccessedAt.After(candidates[j].LastAccessedAt)
	})
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates, nil
}

// scanAll iterates SCAN until cursor exhausted or the effective cap is
// reached. Centralised so every read path shares the same budget logic
// and emits the same truncation signal.
//
// P1 #10 contract:
//
//   - `requested` is the caller's own budget hint (e.g. limit*2 from
//     RetrieveByQuery, or a fixed `r.scanLimit` for Decay). When the
//     caller asks for more than the current `r.scanLimit`, we raise the
//     effective cap up to `maxHotScanLimit` so a high user limit doesn't
//     silently truncate. When the caller asks for less (or 0), we keep
//     the instance default.
//   - `endpoint` is the label propagated into the truncation metric so
//     dashboards can attribute "we're losing keys here" to a specific
//     code path.
//   - If SCAN returns enough keys to hit the effective cap, we emit
//     MemoryHotScanTruncated{endpoint}. That's the operator's signal to
//     raise memory.hot_scan_limit before query results start lying.
//
// The pre-P1-#10 implementation took only `max int` and silently capped
// at min(max, hotScanLimit). RetrieveByType passed overFetch*2 (up to
// 300) but the 200 const won — 100 keys were never examined and the
// caller had no way to know.
func (r *RedisHot) scanAll(ctx context.Context, pattern string, requested int, endpoint string) ([]string, error) {
	cap := r.scanLimit
	if cap <= 0 {
		cap = defaultHotScanLimit
	}
	if requested > cap {
		cap = requested
		if cap > maxHotScanLimit {
			cap = maxHotScanLimit
		}
	}

	keys := make([]string, 0, cap)
	iter := r.client.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
		if len(keys) >= cap {
			break
		}
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	// Truncation signal: we hit the cap AND the cursor likely still had
	// keys to give us. Iter.Next returned false either because we broke
	// out at cap or because cursor finished naturally — we conservatively
	// flag any cap-hit as "potentially truncated" rather than try to
	// read iter.Cursor() (go-redis Iterator doesn't expose it).
	if len(keys) >= cap && endpoint != "" {
		metrics.MemoryHotScanTruncated.WithLabelValues(endpoint).Inc()
	}
	return keys, nil
}

// Delete removes a memory from Redis.
func (r *RedisHot) Delete(ctx context.Context, userID, projectID, memoryID string) error {
	key := fmt.Sprintf("%s:%s", r.keyPrefix(userID, projectID), memoryID)
	return r.client.Del(ctx, key).Err()
}

// PromoteBatch writes a list of Memory entries into hot in a single
// pipeline SET — the P1 #8 read-path back-fill kernel. HybridStore's
// Retrieve enqueues "cold-only hits with score >= threshold" for
// Promote so subsequent retrievals can hit the 5ms hot path instead of
// 50-200ms pgvector.
//
// Semantics:
//   - Default TTL (NewRedisHot's `r.ttl`, normally 24h) — Promote is
//     "this memory matters; keep it cached for a day"; do NOT use
//     KeepTTL (which would carry forward whatever TTL the key had,
//     which here is "no key existed" → -1 → eternal). Setting the
//     fresh TTL is the entire point of Promote.
//   - Best-effort: a JSON marshal error skips that entry but doesn't
//     fail the batch — Promote misses are recoverable on the next
//     Retrieve.
//   - Empty input is a no-op (batcher graceful-shutdown contract).
func (r *RedisHot) PromoteBatch(ctx context.Context, mems []Memory) error {
	if len(mems) == 0 {
		return nil
	}
	pipe := r.client.Pipeline()
	queued := 0
	for i := range mems {
		m := mems[i]
		if m.ID == "" || m.UserID == "" || m.ProjectID == "" {
			continue
		}
		b, err := json.Marshal(&m)
		if err != nil {
			r.logger.Debug("promote marshal failed",
				zap.String("id", m.ID), zap.Error(err))
			continue
		}
		key := fmt.Sprintf("%s:%s", r.keyPrefix(m.UserID, m.ProjectID), m.ID)
		pipe.Set(ctx, key, b, r.ttl)
		queued++
	}
	if queued == 0 {
		return nil
	}
	_, err := pipe.Exec(ctx)
	return err
}

// DeleteBatch removes every (userID, projectID, ID) ref in a single
// pipeline DEL — used by the P1 #7 dedup branch when HybridStore needs
// to evict the non-anchor duplicates from hot after a successful cold
// dedup transaction.
//
// Implementation: reuses TouchRef so callers can pass the exact same
// slice shape as the read-path batcher. We accept multiple keys in one
// DEL command — Redis natively supports DEL key1 key2 …, so the round
// trip is O(1) regardless of len(refs).
//
// Missing keys are silently tolerated (DEL returns the count of keys
// actually removed). That's the desired semantics here: hot may have
// expired the duplicate already, in which case "delete" is already
// satisfied.
func (r *RedisHot) DeleteBatch(ctx context.Context, refs []TouchRef) error {
	if len(refs) == 0 {
		return nil
	}
	keys := make([]string, len(refs))
	for i, ref := range refs {
		keys[i] = fmt.Sprintf("%s:%s", r.keyPrefix(ref.UserID, ref.ProjectID), ref.ID)
	}
	return r.client.Del(ctx, keys...).Err()
}

// DeleteByUser deletes all hot memories belonging to a specific user (GDPR compliance).
func (r *RedisHot) DeleteByUser(ctx context.Context, userID string) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	pattern := fmt.Sprintf("%s:*", r.keyPrefix(userID, "*"))
	
	var cursor uint64
	var totalDeleted int64
	
	for {
		var keys []string
		var err error
		keys, cursor, err = r.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return totalDeleted, err
		}
		
		if len(keys) > 0 {
			res, err := r.client.Del(ctx, keys...).Result()
			if err != nil {
				return totalDeleted, err
			}
			totalDeleted += res
		}
		
		if cursor == 0 {
			break
		}
	}
	
	return totalDeleted, nil
}

// RetrieveByQuery returns memories ranked by cosine similarity to the query embedding.
// Since hot layer is small (< 50 items with 24h TTL), in-memory ranking is acceptable.
//
// Episodic memories are filtered out by default — they are Distiller fuel,
// not user-facing recall candidates. Callers that need to enumerate
// episodic entries from the hot tier must go through
// retrieveByQueryWithEpisodic (currently unused outside the package — the
// Distiller talks directly to the cold tier).
func (r *RedisHot) RetrieveByQuery(ctx context.Context, userID, projectID string, queryEmbedding []float32, limit int) ([]Memory, error) {
	return r.retrieveByQueryFiltered(ctx, userID, projectID, queryEmbedding, limit, true /* excludeEpisodic */)
}

func (r *RedisHot) retrieveByQueryFiltered(ctx context.Context, userID, projectID string, queryEmbedding []float32, limit int, excludeEpisodic bool) ([]Memory, error) {
	pattern := r.keyPrefix(userID, projectID) + ":*"

	// Budget hint = 2x caller's limit. HybridStore.RetrieveByType
	// already passes overFetch*2 (up to 300 for user limit=50), so the
	// effective scan window can grow to 600. Without this, the const
	// 200 floor silently dropped 100 keys before the cosine sort.
	budget := 0
	if limit > 0 {
		budget = limit * 2
	}
	keys, err := r.scanAll(ctx, pattern, budget, "retrieve_by_query")
	if err != nil {
		return nil, err
	}
	metrics.MemoryHotScanKeys.WithLabelValues("retrieve_by_query").Observe(float64(len(keys)))
	if len(keys) == 0 {
		return nil, nil
	}

	pipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.Get(ctx, key)
	}
	_, _ = pipe.Exec(ctx)

	type scored struct {
		memory Memory
		sim    float64
	}
	var results []scored
	for _, cmd := range cmds {
		data, err := cmd.Result()
		if err != nil {
			continue
		}
		var m Memory
		if json.Unmarshal([]byte(data), &m) != nil {
			continue
		}
		if excludeEpisodic && m.Type == MemoryTypeEpisodic {
			continue
		}
		if len(m.Embedding) == 0 {
			continue
		}
		sim := CosineSimilarity(queryEmbedding, m.Embedding)
		results = append(results, scored{memory: m, sim: sim})
	}

	// Primary key: cosine similarity DESC.
	// Tie-breaker: LastAccessedAt DESC. Without it, equal-similarity
	// rows (very common at sim=0 or with quantised embeddings) fell
	// through to map-iteration order — i.e. non-deterministic.
	// Secondary tie-breaker: ID ASC for snapshot determinism.
	sort.Slice(results, func(i, j int) bool {
		if results[i].sim != results[j].sim {
			return results[i].sim > results[j].sim
		}
		if !results[i].memory.LastAccessedAt.Equal(results[j].memory.LastAccessedAt) {
			return results[i].memory.LastAccessedAt.After(results[j].memory.LastAccessedAt)
		}
		return results[i].memory.ID < results[j].memory.ID
	})

	memories := make([]Memory, 0, limit)
	for i := range results {
		if i >= limit {
			break
		}
		memories = append(memories, results[i].memory)
	}
	return memories, nil
}

// Decay is the legacy whole-DB fallback used when no tenant list is
// available (e.g. cold == nil deployments). HybridStore prefers
// DecayTenants — that's the tenant-sliced fast path.
//
// We still cap the scan with hotScanLimit so a misconfigured cluster
// can't pull millions of keys in one go. The pattern is `memory:*`,
// which under SCAN matches every tenant on the instance.
//
// Field semantics: we judge staleness by LastAccessedAt, matching
// PGCold.Decay's SQL (`last_accessed_at < cutoff`). The previous
// implementation used UpdatedAt — a Memory written once and read many
// times would still get decayed because UpdatedAt never advanced. P0
// #4 fixed that for cold; this fixes it for hot too.
//
// demoteThreshold (P1 #8): if > 0, any entry whose post-decay score
// falls below the threshold is DEL'd from hot instead of SET-with-
// reduced-score. Cold keeps the record (the truth) — hot just stops
// caching it because the 5ms hot path is wasted on low-signal entries.
// Pass 0 to disable demotion.
func (r *RedisHot) Decay(ctx context.Context, olderThan time.Duration, factor, demoteThreshold float64) (int, error) {
	// Legacy global-scan path. No caller-supplied budget — use the
	// instance scan limit. Endpoint label distinguishes from the
	// tenant-scoped decay so dashboards can spot "the fallback path
	// is the one being truncated, not the per-tenant one".
	keys, err := r.scanAll(ctx, "memory:*", 0, "decay")
	if err != nil {
		return 0, err
	}
	return r.decayKeys(ctx, keys, olderThan, factor, demoteThreshold)
}

// DecayTenants runs decay for an explicit list of tenants. This is the
// tenant-sliced fast path: each TenantRef yields a SCAN over the
// `memory:<u>:<p>:*` sub-namespace (bounded by hotScanLimit), one
// pipeline GET for all matched keys, and one pipeline SET for the
// stale ones. Net cost: O(N tenants × 2 RTT), independent of the rest
// of the Redis instance.
//
// Errors are per-tenant: a SCAN/GET/SET failure inside tenant X is
// logged and tagged on MemoryDecayHotTenantsTotal{status="err"} but
// does NOT abort the loop — tenant Y still gets its chance. The
// returned count sums decayed entries across all successful tenants;
// the returned error is the first non-nil error encountered (so the
// caller can decide whether to retry or alert).
//
// demoteThreshold (P1 #8): see Decay docstring.
func (r *RedisHot) DecayTenants(ctx context.Context, tenants []TenantRef, olderThan time.Duration, factor, demoteThreshold float64) (int, error) {
	total := 0
	var firstErr error
	for _, t := range tenants {
		if t.UserID == "" || t.ProjectID == "" {
			metrics.MemoryDecayHotTenantsTotal.WithLabelValues("skip").Inc()
			continue
		}
		start := time.Now()
		n, err := r.decayTenant(ctx, t.UserID, t.ProjectID, olderThan, factor, demoteThreshold)
		metrics.MemoryDecayHotBatchDuration.Observe(time.Since(start).Seconds())
		if err != nil {
			metrics.MemoryDecayHotTenantsTotal.WithLabelValues("err").Inc()
			r.logger.Warn("hot decay tenant failed",
				zap.String("user_id", t.UserID),
				zap.String("project_id", t.ProjectID),
				zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		metrics.MemoryDecayHotTenantsTotal.WithLabelValues("ok").Inc()
		total += n
	}
	return total, firstErr
}

// decayTenant is the per-tenant kernel: SCAN within
// `memory:<u>:<p>:*` (budget=hotScanLimit), pipeline GET to load each
// candidate, then pipeline SET with KeepTTL for the ones that need
// their score reduced. KeepTTL is critical — touch-style refresh is
// not "this memory is fresh again", just "its score is now lower".
func (r *RedisHot) decayTenant(ctx context.Context, userID, projectID string, olderThan time.Duration, factor, demoteThreshold float64) (int, error) {
	pattern := r.keyPrefix(userID, projectID) + ":*"
	// Decay scans a whole tenant once per tick; the instance scanLimit
	// is the right budget. Endpoint label "decay_tenant" so dashboards
	// separate this from the legacy global Decay path.
	keys, err := r.scanAll(ctx, pattern, 0, "decay_tenant")
	if err != nil {
		return 0, err
	}
	metrics.MemoryDecayHotScanKeys.Observe(float64(len(keys)))
	if len(keys) == 0 {
		return 0, nil
	}
	return r.decayKeys(ctx, keys, olderThan, factor, demoteThreshold)
}

// decayKeys is shared by Decay (legacy fallback) and decayTenant.
// Two-pipeline pattern matches TouchBatch: GET all, then either SET or
// DEL the affected subset (one branch per key). KeepTTL preserves the
// 24h hot expiry on SET — a decay event is "score dropped", not
// "freshly written".
//
// Stop conditions:
//   - empty keys: no-op return (0, nil)
//   - score <= 0.01: skip entirely (matches PGCold.Decay floor)
//   - LastAccessedAt >= cutoff: skip (not stale yet)
//
// P1 #8 demote branch: when demoteThreshold > 0, an entry whose new
// score (m.Score * factor) falls below the threshold is DEL'd from hot
// instead of SET. The cold copy still holds the truth — hot just stops
// caching low-signal entries so RetrieveByQuery's tier-1 results stay
// clean. The DEL fires only when we cross the threshold *this iteration*
// (m.Score >= demoteThreshold > newScore) so we don't keep DEL'ing keys
// that are already below threshold from a previous Decay run.
//
// Decay sets UpdatedAt = now so callers can see "when was this entry
// last decayed?" — handy for debugging weird score timelines. We do
// NOT touch LastAccessedAt: decay is a system event, not a user read.
//
// Returns the total number of affected keys (SET + DEL combined).
func (r *RedisHot) decayKeys(ctx context.Context, keys []string, olderThan time.Duration, factor, demoteThreshold float64) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	getPipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = getPipe.Get(ctx, key)
	}
	_, _ = getPipe.Exec(ctx)

	cutoff := time.Now().Add(-olderThan)
	now := time.Now()
	writePipe := r.client.Pipeline()
	count := 0
	demoted := 0
	for i, cmd := range cmds {
		data, err := cmd.Result()
		if err != nil {
			continue
		}
		var m Memory
		if json.Unmarshal([]byte(data), &m) != nil {
			continue
		}
		// Score floor — once we're at-or-below 0.01 the decayed
		// number is indistinguishable from noise; stop bleeding it.
		if m.Score <= 0.01 {
			continue
		}
		if !m.LastAccessedAt.Before(cutoff) {
			continue
		}
		newScore := m.Score * factor

		// P1 #8 demote: if newScore crosses below the threshold this
		// iteration, evict from hot. Cold UPDATE still happens in
		// PGCold.Decay (set-based), so cold remains the source of
		// truth — we just stop caching here.
		if demoteThreshold > 0 && m.Score >= demoteThreshold && newScore < demoteThreshold {
			writePipe.Del(ctx, keys[i])
			count++
			demoted++
			continue
		}

		m.Score = newScore
		m.UpdatedAt = now
		b, err := json.Marshal(&m)
		if err != nil {
			continue
		}
		writePipe.Set(ctx, keys[i], b, redis.KeepTTL)
		count++
	}
	if count == 0 {
		return 0, nil
	}
	if _, err := writePipe.Exec(ctx); err != nil {
		return 0, err
	}
	if demoted > 0 {
		metrics.MemoryDemoteTotal.WithLabelValues("hot").Add(float64(demoted))
	}
	return count, nil
}
