package memory

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// promoteRecorder mirrors flushRecorder in the access batcher test:
// captures every batch the loop hands to its flush callback so tests
// can assert size, dedup, ordering, and error resilience.
type promoteRecorder struct {
	mu       sync.Mutex
	batches  [][]Memory
	failNext atomic.Bool
}

func (p *promoteRecorder) flush(mems []Memory) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failNext.Swap(false) {
		return errInjectedFlushFail
	}
	cp := make([]Memory, len(mems))
	copy(cp, mems)
	p.batches = append(p.batches, cp)
	return nil
}

func (p *promoteRecorder) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, b := range p.batches {
		n += len(b)
	}
	return n
}

func (p *promoteRecorder) idsSnapshot() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]string, len(p.batches))
	for i, b := range p.batches {
		ids := make([]string, len(b))
		for j, m := range b {
			ids[j] = m.ID
		}
		out[i] = ids
	}
	return out
}

// TestRunPromoteBatcherLoop_FlushOnBatchSize: hitting BatchSize must
// trigger a flush immediately. Without this, low-QPS deployments would
// see promotes pile up in the queue and the hot tier would never warm.
func TestRunPromoteBatcherLoop_FlushOnBatchSize(t *testing.T) {
	rec := &promoteRecorder{}
	queue := make(chan Memory, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runPromoteBatcherLoop(ctx, queue, 3, 1*time.Hour, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- Memory{ID: "a", UserID: "u", ProjectID: "p", Score: 0.9}
	queue <- Memory{ID: "b", UserID: "u", ProjectID: "p", Score: 0.9}
	queue <- Memory{ID: "c", UserID: "u", ProjectID: "p", Score: 0.9}

	require.Eventually(t, func() bool { return rec.total() == 3 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	snap := rec.idsSnapshot()
	require.Len(t, snap, 1, "exactly one flush expected")
	assert.Equal(t, []string{"a", "b", "c"}, snap[0])
}

// TestRunPromoteBatcherLoop_FlushOnTimer: a partial batch should flush
// when the interval elapses — otherwise low-traffic deployments would
// never warm the hot tier.
func TestRunPromoteBatcherLoop_FlushOnTimer(t *testing.T) {
	rec := &promoteRecorder{}
	queue := make(chan Memory, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runPromoteBatcherLoop(ctx, queue, 100, 50*time.Millisecond, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- Memory{ID: "only", UserID: "u", ProjectID: "p", Score: 0.9}
	require.Eventually(t, func() bool { return rec.total() == 1 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	snap := rec.idsSnapshot()
	require.GreaterOrEqual(t, len(snap), 1)
	assert.Equal(t, []string{"only"}, snap[0])
}

// TestRunPromoteBatcherLoop_DedupWithinBatch: enqueueing the same ID
// twice within a single batch window must produce only one Promote —
// hot doesn't need to re-SET the same JSON, and dedup keeps the batch
// shorter.
func TestRunPromoteBatcherLoop_DedupWithinBatch(t *testing.T) {
	rec := &promoteRecorder{}
	queue := make(chan Memory, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runPromoteBatcherLoop(ctx, queue, 100, 30*time.Millisecond, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- Memory{ID: "x", UserID: "u", ProjectID: "p", Score: 0.9}
	queue <- Memory{ID: "x", UserID: "u", ProjectID: "p", Score: 0.9}
	queue <- Memory{ID: "x", UserID: "u", ProjectID: "p", Score: 0.9}
	queue <- Memory{ID: "y", UserID: "u", ProjectID: "p", Score: 0.9}

	require.Eventually(t, func() bool { return rec.total() == 2 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	snap := rec.idsSnapshot()
	require.GreaterOrEqual(t, len(snap), 1)
	assert.ElementsMatch(t, []string{"x", "y"}, snap[0])
}

// TestRunPromoteBatcherLoop_DrainsOnContextCancel: graceful shutdown
// must NOT lose the last partial batch — main.go relies on this so
// the final 5s of promote signal survives SIGTERM.
func TestRunPromoteBatcherLoop_DrainsOnContextCancel(t *testing.T) {
	rec := &promoteRecorder{}
	queue := make(chan Memory, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runPromoteBatcherLoop(ctx, queue, 100, 1*time.Hour, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- Memory{ID: "last1", UserID: "u", ProjectID: "p", Score: 0.9}
	queue <- Memory{ID: "last2", UserID: "u", ProjectID: "p", Score: 0.9}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	snap := rec.idsSnapshot()
	require.Len(t, snap, 1, "exactly one final flush on shutdown")
	assert.ElementsMatch(t, []string{"last1", "last2"}, snap[0])
}

// TestRunPromoteBatcherLoop_FlushErrorDoesNotKillLoop: PromoteBatch
// errors (Redis hiccup) must NOT take down the batcher — the next
// flush should still attempt to run. Otherwise a transient Redis
// outage would stop all warm-fill until restart.
func TestRunPromoteBatcherLoop_FlushErrorDoesNotKillLoop(t *testing.T) {
	rec := &promoteRecorder{}
	rec.failNext.Store(true)
	queue := make(chan Memory, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runPromoteBatcherLoop(ctx, queue, 2, 30*time.Millisecond, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- Memory{ID: "a", UserID: "u", ProjectID: "p", Score: 0.9}
	queue <- Memory{ID: "b", UserID: "u", ProjectID: "p", Score: 0.9}
	queue <- Memory{ID: "c", UserID: "u", ProjectID: "p", Score: 0.9}
	queue <- Memory{ID: "d", UserID: "u", ProjectID: "p", Score: 0.9}

	require.Eventually(t, func() bool { return rec.total() >= 2 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	snap := rec.idsSnapshot()
	require.GreaterOrEqual(t, len(snap), 1)
	assert.Equal(t, []string{"c", "d"}, snap[0],
		"first batch errored and was dropped; second batch must still flush")
}

// TestEnqueuePromote_OnlyColdOnlyHits: hot-already-cached entries must
// NOT be re-promoted — we'd waste a SET on what's already there. The
// hot slice from the read path identifies entries that were tier-1
// hits; only fused entries not in `hot` are candidates.
func TestEnqueuePromote_OnlyColdOnlyHits(t *testing.T) {
	h := &HybridStore{
		logger:       zap.NewNop(),
		hot:          &RedisHot{}, // non-nil so the early-return is bypassed
		promoteQueue: make(chan Memory, 8),
		promoteOpts:  PromoteOptions{Threshold: 0.7},
	}

	hot := []Memory{
		{ID: "hot1", Score: 0.95},
		{ID: "hot2", Score: 0.9},
	}
	fused := []Memory{
		{ID: "hot1", Score: 0.95}, // skip — already in hot
		{ID: "hot2", Score: 0.9},  // skip — already in hot
		{ID: "cold1", Score: 0.9}, // promote — cold-only hit, above threshold
		{ID: "cold2", Score: 0.5}, // skip — below threshold
	}
	h.enqueuePromote(hot, fused)

	require.Equal(t, 1, len(h.promoteQueue), "exactly cold1 should be queued")
	got := <-h.promoteQueue
	assert.Equal(t, "cold1", got.ID)
}

// TestEnqueuePromote_ThresholdFilter: low-score memories are not worth
// the hot tier's 24h footprint. The threshold filter must apply.
func TestEnqueuePromote_ThresholdFilter(t *testing.T) {
	h := &HybridStore{
		logger:       zap.NewNop(),
		hot:          &RedisHot{},
		promoteQueue: make(chan Memory, 8),
		promoteOpts:  PromoteOptions{Threshold: 0.7},
	}

	fused := []Memory{
		{ID: "low1", Score: 0.5}, // skip — below 0.7
		{ID: "low2", Score: 0.69},
		{ID: "high1", Score: 0.7},  // promote — at threshold
		{ID: "high2", Score: 0.99}, // promote
	}
	h.enqueuePromote(nil, fused)

	require.Equal(t, 2, len(h.promoteQueue))
	ids := map[string]bool{}
	close(h.promoteQueue)
	for m := range h.promoteQueue {
		ids[m.ID] = true
	}
	assert.True(t, ids["high1"])
	assert.True(t, ids["high2"])
}

// TestEnqueuePromote_NonBlockingOnFullQueue: the read path MUST NOT
// block on the promote queue. When the queue is full, drops are
// counted but the caller proceeds unblocked.
func TestEnqueuePromote_NonBlockingOnFullQueue(t *testing.T) {
	h := &HybridStore{
		logger:       zap.NewNop(),
		hot:          &RedisHot{},
		promoteQueue: make(chan Memory, 2), // tiny on purpose
		promoteOpts:  PromoteOptions{Threshold: 0.7},
	}

	fused := []Memory{
		{ID: "a", Score: 0.9},
		{ID: "b", Score: 0.9},
		{ID: "c", Score: 0.9}, // drops — queue full
		{ID: "d", Score: 0.9}, // drops — queue full
	}
	t0 := time.Now()
	h.enqueuePromote(nil, fused)
	elapsed := time.Since(t0)

	assert.Less(t, elapsed, 50*time.Millisecond,
		"enqueuePromote must be non-blocking even when queue is full")
	assert.Equal(t, 2, len(h.promoteQueue), "queue should be exactly full (cap=2)")
}

// TestEnqueuePromote_NilQueueIsNoOp: when EnablePromoteBatcher hasn't
// been called the read path should silently skip enqueue — don't blow
// up on nil chan deref. This mirrors enqueueTouches behavior.
func TestEnqueuePromote_NilQueueIsNoOp(t *testing.T) {
	h := &HybridStore{logger: zap.NewNop(), hot: &RedisHot{}} // promoteQueue == nil
	require.NotPanics(t, func() {
		h.enqueuePromote(nil, []Memory{{ID: "x", Score: 0.9}})
	})
}

// TestEnqueuePromote_NilHotIsNoOp: if no hot tier is configured,
// promote makes no sense — the early return is a defensive guard.
func TestEnqueuePromote_NilHotIsNoOp(t *testing.T) {
	h := &HybridStore{
		logger:       zap.NewNop(),
		promoteQueue: make(chan Memory, 4),
		promoteOpts:  PromoteOptions{Threshold: 0.7},
	}
	require.NotPanics(t, func() {
		h.enqueuePromote(nil, []Memory{{ID: "x", Score: 0.9}})
	})
	assert.Equal(t, 0, len(h.promoteQueue))
}

// TestEnqueuePromote_IgnoresEmptyIDs: defense against producers that
// somehow yield zero-ID memories. PromoteBatch's per-entry validation
// would catch this but enqueue is the cheaper place to filter.
func TestEnqueuePromote_IgnoresEmptyIDs(t *testing.T) {
	h := &HybridStore{
		logger:       zap.NewNop(),
		hot:          &RedisHot{},
		promoteQueue: make(chan Memory, 4),
		promoteOpts:  PromoteOptions{Threshold: 0.7},
	}
	h.enqueuePromote(nil, []Memory{
		{ID: "", Score: 0.9},
		{ID: "real", Score: 0.9},
		{ID: "", Score: 0.95},
	})
	assert.Equal(t, 1, len(h.promoteQueue))
}

// TestPromoteOptions_Defaults locks in the configured-for-prod values
// so a future refactor doesn't silently flip the threshold from 0.7
// to 0 (which would flood hot with noise).
func TestPromoteOptions_Defaults(t *testing.T) {
	o := PromoteOptions{}.withDefaults()
	assert.InDelta(t, 0.7, o.Threshold, 0.001)
	assert.Greater(t, o.BatchSize, 0)
	assert.Greater(t, o.FlushInterval, time.Duration(0))
	assert.Greater(t, o.QueueSize, 0)
}
