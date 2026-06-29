package memory

import (
	"context"
	"time"

	"github.com/agent/code_agent/internal/metrics"
	"go.uber.org/zap"
)

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
