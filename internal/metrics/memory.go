package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ─── Memory Subsystem Metrics ───────────────────────────────────────────────
//
// Labels are kept low-cardinality on purpose:
//   - `op`         ∈ {store, retrieve, list, decay, promote, demote}
//   - `tier`       ∈ {hot, cold, hybrid}    (which physical layer was hit)
//   - `status`     ∈ {ok, err}              (caller-observed outcome)
//   - `mem_type`   ∈ {preference, decision, knowledge, pattern}
//   - `action`     ∈ {added, merged, updated, …}  (blackboard payload kind)
//
// Crucially we DO NOT label by user_id / project_id — that would blow the
// cardinality budget on a multi-tenant deployment (the metrics.go header
// spells out this rule). If a per-tenant breakdown is ever needed, expose
// it via a separate `_per_tenant_top10` histogram of expensive memories.

var (
	// MemoryStoreTotal counts long-term memory write operations.
	MemoryStoreTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "store_total",
		Help:      "Total number of memory store operations",
	}, []string{"tier", "status", "mem_type"})

	// MemoryStoreDuration observes Store() latency end-to-end (includes
	// embedding generation + conflict KNN + double-write).
	MemoryStoreDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "store_duration_seconds",
		Help:      "End-to-end latency of HybridStore.Store",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"tier", "status"})

	// MemoryRetrieveTotal counts retrieval requests.
	MemoryRetrieveTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "retrieve_total",
		Help:      "Total number of memory retrieve operations",
	}, []string{"tier", "status"})

	// MemoryRetrieveDuration observes Retrieve() latency.
	MemoryRetrieveDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "retrieve_duration_seconds",
		Help:      "Latency of HybridStore.Retrieve",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2},
	}, []string{"tier"})

	// MemoryRetrieveResultCount tracks how many memories we returned per
	// call. Lets dashboards spot "we're always returning 0" (retrieval
	// silently broken) and "we always max out at limit" (under-served).
	MemoryRetrieveResultCount = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "retrieve_result_count",
		Help:      "Number of memories returned per retrieve",
		Buckets:   []float64{0, 1, 2, 3, 5, 10, 20},
	})

	// MemoryConflictTotal counts conflict resolutions per outcome.
	// outcome: "merge" (reinforcement), "override" (new beats old),
	//          "preserve" (old high-score wins), "none" (no conflict found),
	//          "dedup" (P1 #7: anchor absorbed >= 2 conflicts, dups deleted).
	MemoryConflictTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "conflict_total",
		Help:      "Total number of conflict-resolution decisions",
	}, []string{"outcome"})

	// MemoryDedupTotal counts how often extractor.isDuplicate fired.
	// method: "embedding" | "ngram"
	MemoryDedupTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "dedup_total",
		Help:      "Total number of duplicate-memory rejections",
	}, []string{"method"})

	// MemoryDedupRemovedTotal counts how many existing duplicates were
	// deleted by HybridStore's dedup branch (P1 #7). Different from
	// MemoryDedupTotal which counts pre-store rejections — this is the
	// post-store cleanup of memories that already made it into PG.
	MemoryDedupRemovedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "dedup_removed_total",
		Help:      "Existing PG memories deleted by HybridStore dedup branch",
	})

	// MemoryDedupBatchSize tracks how many dups were collapsed per
	// Store call. Healthy steady-state is 0-1; sustained > 5 suggests
	// upstream (Extractor / RecordTaskEpisode) is producing redundant
	// memories and the threshold needs tightening, not the dedup logic.
	MemoryDedupBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "dedup_batch_size",
		Help:      "Number of duplicates removed in a single Store call",
		Buckets:   []float64{1, 2, 3, 5, 10, 20, 32},
	})

	// MemoryDedupCandidateCount tracks how many candidates the P1 #9
	// Extractor.isDuplicate path actually inspected per call. Sized to
	// surface "the K is too tight": P95 ≈ dedupCandidateLimit means
	// real near-duplicates are likely being missed beyond the cutoff,
	// and the operator should raise memory.dedup_candidate_limit.
	//
	// Reading the histogram:
	//   - P50 close to 0: tenant library is small; dedup K is fine
	//   - P95 << limit: library is moderate; K is sufficient
	//   - P95 ≈ limit: library is saturating the K; consider raising
	//   - P95 = limit and dedup_total{embedding} flatlining: K too low,
	//     real dupes are slipping past — raise dedup_candidate_limit
	MemoryDedupCandidateCount = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "dedup_candidate_count",
		Help:      "Number of candidates examined by Extractor.isDuplicate per call",
		Buckets:   []float64{0, 1, 5, 10, 20, 30, 50, 100, 200},
	})

	// MemoryDecayRunsTotal tracks every Decay() invocation.
	MemoryDecayRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "decay_runs_total",
		Help:      "Total number of decay scheduler iterations",
	}, []string{"status"})

	// MemoryDecayAffected reports per-iteration rows touched. Histogram
	// (not gauge) so we can see distribution over time, not just last value.
	MemoryDecayAffected = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "decay_affected_count",
		Help:      "Number of memories whose score was decayed per iteration",
		Buckets:   []float64{0, 1, 5, 25, 100, 500, 2500, 10000},
	})

	// MemoryDecayHotTenantsTotal tracks the per-tenant outcome of the
	// hot-tier decay loop (P1 #6). status="ok"|"err"|"skip".
	// "skip" fires for malformed TenantRef (empty UserID/ProjectID); it
	// should never happen in healthy traffic and is therefore a useful
	// red-flag metric on dashboards.
	MemoryDecayHotTenantsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "decay_hot_tenants_total",
		Help:      "Hot-tier decay outcomes per tenant slice",
	}, []string{"status"})

	// MemoryDecayHotScanKeys observes how many hot-tier keys SCAN
	// returned for each per-tenant decay slice. Helps catch tenants
	// that hit the hotScanLimit ceiling (=200), which would mean some
	// stale entries are escaping decay until the next tick.
	MemoryDecayHotScanKeys = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "decay_hot_scan_keys",
		Help:      "Hot-tier keys SCAN returned per tenant decay slice",
		Buckets:   []float64{1, 5, 10, 25, 50, 100, 200, 500},
	})

	// MemoryDecayHotBatchDuration measures end-to-end time per tenant
	// slice (SCAN + pipeline GET + pipeline SET). Sub-100ms is healthy
	// on a Redis with ≤ 50 keys/tenant; tail latency here surfaces
	// network or Redis contention issues.
	MemoryDecayHotBatchDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "decay_hot_batch_duration_seconds",
		Help:      "Per-tenant hot decay duration in seconds",
		Buckets:   []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	})

	// MemoryPromoteTotal counts batcher flush outcomes (P1 #8).
	// status="ok"|"err". A persistent "err" rate indicates the hot
	// tier is unhealthy — operators should check the Redis client.
	MemoryPromoteTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "promote_total",
		Help:      "Cold→hot promotion batch outcomes",
	}, []string{"status"})

	// MemoryPromoteBatchSize tracks how many entries each PromoteBatch
	// SET pipeline carried. Steady-state distribution skewed to small
	// values is expected (high cache-hit rate); persistent saturation
	// at BatchSize → threshold is too low / read traffic is unusually
	// cold-skewed.
	MemoryPromoteBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "promote_batch_size",
		Help:      "Items per cold→hot promote batch flush",
		Buckets:   []float64{1, 5, 10, 25, 50, 100},
	})

	// MemoryPromoteQueueDropsTotal counts non-blocking enqueue failures.
	// > 1% of retrieve QPS sustained = QueueSize too small (or the
	// batcher goroutine is jammed / not running).
	MemoryPromoteQueueDropsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "promote_queue_drops_total",
		Help:      "Read-path promote enqueue failures (queue full)",
	})

	// MemoryDemoteTotal counts hot evictions from the Decay path
	// (P1 #8). tier="hot" — currently the only demote source. Kept
	// as CounterVec to leave room for future tiers (e.g. "explicit"
	// when an operator tool fires Demote manually).
	MemoryDemoteTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "demote_total",
		Help:      "Memories evicted from hot tier",
	}, []string{"tier"})

	// MemoryExtractorRunsTotal counts ExtractFromInteraction invocations.
	// path: "llm" | "heuristic" | "skipped"
	MemoryExtractorRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "extractor_runs_total",
		Help:      "Total memory extractions by path taken",
	}, []string{"path"})

	// MemoryExtractorStored tracks how many memories we persisted per run.
	MemoryExtractorStored = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "extractor_stored_per_run",
		Help:      "Memories stored per ExtractFromInteraction invocation",
		Buckets:   []float64{0, 1, 2, 3, 5, 10},
	})

	// MemoryBlackboardPublishTotal tracks blackboard pub/sub usage.
	// status: "ok" | "err" | "dropped" (subscriber channel full)
	MemoryBlackboardPublishTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "blackboard_publish_total",
		Help:      "Total blackboard publish attempts",
	}, []string{"action", "status"})

	// MemoryBlackboardDroppedTotal counts subscriber-side drops (channel
	// full). Separate counter because publisher-side success ≠ delivery,
	// and a quiet "events being silently dropped" failure mode used to be
	// invisible (only a Warn log).
	MemoryBlackboardDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "blackboard_dropped_total",
		Help:      "Total blackboard events dropped due to slow subscriber",
	})

	// MemoryDistillRunsTotal counts every Distiller.Distill invocation.
	// path: "ok" | "skipped" | "empty_llm" | "error_list" | "error_llm" | "error_store"
	// Splitting "skipped" from "ok" is critical: a healthy system with low
	// traffic will skip most ticks, and that should not look like an outage.
	MemoryDistillRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "distill_runs_total",
		Help:      "Total Distiller invocations by outcome",
	}, []string{"path"})

	// MemoryDistillDuration tracks end-to-end distillation latency
	// (list → LLM → store). LLM call dominates, so the buckets are wide.
	MemoryDistillDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "distill_duration_seconds",
		Help:      "End-to-end latency of Distiller.Distill",
		Buckets:   []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60},
	})

	// MemoryDistillProduced counts memories the Distiller wrote back.
	// kind: "semantic" (only kind today; reserved for future "summary",
	// "skill", etc.).
	MemoryDistillProduced = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "distill_produced_total",
		Help:      "Total memories produced by Distiller",
	}, []string{"kind"})

	// MemoryDistillTargetsTotal counts tenants the scheduler considered
	// per tick, split by where they came from. Critical for diagnosing
	// "auto-discovery never finds anything" (= source=discovered stays
	// at 0) vs. "operator yaml is the only thing driving distillation"
	// (= source=static dominates).
	// source: "static" | "discovered" | "merged"
	MemoryDistillTargetsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "distill_targets_total",
		Help:      "Tenants the distill scheduler considered per tick by source",
	}, []string{"source"})

	// MemoryDistillDiscoverDuration observes how long the PG GROUP BY
	// took for ListActiveDistillTenants. With the partial index in
	// place this should sit < 50 ms even at 10k tenants; sudden P99
	// growth is the early-warning sign that the index is missing or
	// the table is unusual large.
	MemoryDistillDiscoverDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "distill_discover_duration_seconds",
		Help:      "Latency of distill tenant auto-discovery query",
		Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2},
	})

	// MemoryTouchBatchTotal counts read-path access-touch flushes by
	// outcome. status="ok"|"err". A spike in err is the canonical
	// "Decay accuracy is silently degrading" alarm.
	MemoryTouchBatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "touch_batch_total",
		Help:      "Total HybridStore access-touch batch flushes",
	}, []string{"status"})

	// MemoryTouchBatchSize observes how many IDs went into a single
	// batch flush. Useful for sizing BatchSize / FlushInterval — if
	// P50 << BatchSize, the interval dominates; if P95 hits BatchSize
	// the size dominates and read traffic is producing back-pressure.
	MemoryTouchBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "touch_batch_size",
		Help:      "Number of memory IDs per touch-batch flush",
		Buckets:   []float64{1, 5, 10, 25, 50, 100, 200, 500},
	})

	// MemoryTouchQueueDropsTotal counts read-side IDs dropped because
	// the in-memory touch queue was full. Dropping is non-destructive
	// (the underlying memory still exists; we just skip one access
	// update) but accumulating drops biases Decay against frequently-
	// accessed memories, so a non-zero rate is a tuning signal:
	// raise QueueSize or lower FlushInterval.
	MemoryTouchQueueDropsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "touch_queue_drops_total",
		Help:      "Total memory IDs dropped due to a full touch queue",
	})

	// MemoryHotScanKeys observes the number of keys returned by RedisHot's
	// SCAN per Retrieve / RetrieveByQuery call. Documented invariant: hot
	// tier holds ≤ 50 entries per (user, project) thanks to 24h TTL — but
	// no code path *enforces* this. If a high-traffic tenant breaches it,
	// older entries used to be silently truncated; with this histogram we
	// can spot the regression on dashboards instead of in a bug report.
	// endpoint: "retrieve" | "retrieve_by_query"
	MemoryHotScanKeys = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "hot_scan_keys",
		Help:      "Number of hot-tier keys SCAN returned per retrieval",
		Buckets:   []float64{1, 5, 10, 25, 50, 100, 200, 500, 1000, 2000},
	}, []string{"endpoint"})

	// MemoryHotScanTruncated counts P1 #10 truncation events: every time
	// RedisHot.scanAll fills its effective cap and stops iterating
	// before SCAN's cursor naturally exhausts. The pre-fix code path
	// did exactly this silently — operators only found out when
	// "missing memory" bugs landed in support.
	//
	// endpoint: "retrieve" | "retrieve_by_query" | "decay" | "decay_tenant"
	//
	// Reading the counter:
	//   - Sustained 0 → scan budget is comfortable for current load
	//   - Spikes on "retrieve_by_query" only → user load grew past
	//     overFetch*2 × default ceiling; raise memory.hot_scan_limit
	//   - Steady on "decay_tenant" → a single tenant exceeds the cap
	//     even at decay time; the 50/tenant invariant is being
	//     systematically violated, investigate write churn
	MemoryHotScanTruncated = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "hot_scan_truncated_total",
		Help:      "Times RedisHot.scanAll hit its cap (some keys not examined)",
	}, []string{"endpoint"})

	// MemoryFeedbackTotal counts user thumbs-up/down on assistant messages
	// that cited at least one memory. direction: positive | negative.
	MemoryFeedbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "feedback_total",
		Help:      "User feedback events that adjusted cited memory scores",
	}, []string{"direction"})

	// MemoryCitationBoostTotal counts automatic score bumps from LLM citations.
	MemoryCitationBoostTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "citation_boost_total",
		Help:      "Automatic score adjustments from [mem:id] citations in assistant replies",
	}, []string{"source", "status"})

	// MemoryCitationFeedbackTotal tracks turns where memories were injected into
	// the prompt vs whether the assistant cited any [mem:id] tags (REAUDIT-P0-3).
	// outcome:
	//   - injected: at least one memory was injected this turn
	//   - missed:   injected > 0 but response contained zero citations
	//   - cited:    injected > 0 and response cited at least one id
	//   - partial:  injected > 0, some cited but not all injected ids cited
	MemoryCitationFeedbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "citation_feedback_total",
		Help:      "Citation feedback loop outcomes when memories were injected (REAUDIT-P0-3 observability)",
	}, []string{"outcome"})

	// MemoryRetrieveDegradedTotal counts retrieve calls that fell back from
	// semantic search to ILIKE because the embedder was unavailable or
	// returned an error (REAUDIT-P0-4).
	// reason: embedder_nil | embedder_failed
	MemoryRetrieveDegradedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "retrieve_degraded_total",
		Help:      "Retrieve paths degraded from vector search to ILIKE (REAUDIT-P0-4)",
	}, []string{"reason"})

	// MemoryFailuresTotal is the AUDIT-P2-4 alertable error-classification
	// counter. Previously a hot-store error and a cold-store error were
	// both logged at Warn / Error with no machine-readable severity, so
	// "data was just lost" and "cache miss happened" looked identical in
	// dashboards.
	//
	// Labels:
	//   - tier:     "hot" | "cold" | "blackboard" | "embedder"
	//   - op:       "store" | "retrieve" | "list" | "publish" | "touch" | "decay"
	//   - severity: "warn"     -> degraded, compensating path exists (e.g. hot miss while cold is fine)
	//               "error"    -> degraded with no compensating path (e.g. cold retrieve failed
	//                             so dedup K-NN missed candidates this call)
	//               "critical" -> primary write to source-of-truth (cold) failed; data was lost.
	//                             PromQL rule of thumb: `rate(memory_failures_total{severity="critical"}[5m]) > 0` → page.
	MemoryFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "memory",
		Name:      "failures_total",
		Help:      "Memory subsystem failures by tier/op/severity (AUDIT-P2-4 alertable error classification)",
	}, []string{"tier", "op", "severity"})
)

// init pre-creates every documented {tier, op, severity} child of
// MemoryFailuresTotal so the counter is visible in /metrics from the
// first scrape onward — without this, prometheus would only emit a
// counter row after the *first* increment, so an alert rule referencing
// `code_agent_memory_failures_total{severity="critical"} == 0` would
// trigger spurious "no data" alerts on a healthy fresh deployment.
func init() {
	warm := []struct{ tier, op, severity string }{
		{"cold", "store", "critical"},
		{"hot", "store", "warn"},
		{"cold", "retrieve", "error"},
		{"hot", "retrieve", "warn"},
		{"cold", "list", "error"},
		{"hot", "list", "warn"},
		{"blackboard", "publish", "warn"},
		{"embedder", "embed", "warn"},
		{"embedder", "embed", "error"},
	}
	for _, w := range warm {
		MemoryFailuresTotal.WithLabelValues(w.tier, w.op, w.severity).Add(0)
	}
	for _, reason := range []string{"embedder_nil", "embedder_failed"} {
		MemoryRetrieveDegradedTotal.WithLabelValues(reason).Add(0)
	}
}
