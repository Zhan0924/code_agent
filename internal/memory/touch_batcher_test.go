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

// flushRecorder captures every batch the loop hands to flush, so tests
// can assert on order, dedup, and timing. Implemented as a slice +
// mutex (not a chan) because some tests want to inspect partial state
// before the loop exits.
type flushRecorder struct {
	mu       sync.Mutex
	batches  [][]TouchRef
	failNext atomic.Bool
}

func (f *flushRecorder) flush(refs []TouchRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext.Swap(false) {
		return errInjectedFlushFail
	}
	cp := make([]TouchRef, len(refs))
	copy(cp, refs)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *flushRecorder) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

// idsSnapshot returns each captured batch as []string of IDs — most
// tests only care about ID ordering, not the (user, project) prefix.
func (f *flushRecorder) idsSnapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.batches))
	for i, b := range f.batches {
		ids := make([]string, len(b))
		for j, r := range b {
			ids[j] = r.ID
		}
		out[i] = ids
	}
	return out
}

// refsSnapshot returns batches verbatim — for tests that assert
// UserID/ProjectID round-trip through the loop.
func (f *flushRecorder) refsSnapshot() [][]TouchRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]TouchRef, len(f.batches))
	for i, b := range f.batches {
		cp := make([]TouchRef, len(b))
		copy(cp, b)
		out[i] = cp
	}
	return out
}

// refOf builds a TouchRef with arbitrary (user, project, id).
func refOf(u, p, id string) TouchRef {
	return TouchRef{UserID: u, ProjectID: p, ID: id}
}

// idRef builds a TouchRef with only ID populated — for tests that
// don't care about hot-key reconstruction.
func idRef(id string) TouchRef {
	return TouchRef{ID: id}
}

var errInjectedFlushFail = errFlush{"injected"}

type errFlush struct{ msg string }

func (e errFlush) Error() string { return e.msg }

// TestRunAccessBatcherLoop_FlushOnBatchSize: filling the batch must
// trigger a flush immediately, not wait for the timer.
func TestRunAccessBatcherLoop_FlushOnBatchSize(t *testing.T) {
	rec := &flushRecorder{}
	queue := make(chan TouchRef, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runAccessBatcherLoop(ctx, queue, 3, 1*time.Hour, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- idRef("a")
	queue <- idRef("b")
	queue <- idRef("c") // hits batchSize=3 → triggers flush

	require.Eventually(t, func() bool { return rec.total() == 3 }, 2*time.Second, 5*time.Millisecond)

	cancel()
	<-done

	snap := rec.idsSnapshot()
	require.Len(t, snap, 1, "exactly one flush expected")
	assert.Equal(t, []string{"a", "b", "c"}, snap[0])
}

// TestRunAccessBatcherLoop_FlushOnTimer: a partial batch should flush
// when the interval elapses — otherwise low-traffic deployments would
// never advance last_accessed_at.
func TestRunAccessBatcherLoop_FlushOnTimer(t *testing.T) {
	rec := &flushRecorder{}
	queue := make(chan TouchRef, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runAccessBatcherLoop(ctx, queue, 100, 50*time.Millisecond, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- idRef("only")
	require.Eventually(t, func() bool { return rec.total() == 1 }, 2*time.Second, 5*time.Millisecond)

	cancel()
	<-done

	snap := rec.idsSnapshot()
	require.GreaterOrEqual(t, len(snap), 1)
	assert.Equal(t, []string{"only"}, snap[0])
}

// TestRunAccessBatcherLoop_DedupWithinBatch: enqueueing the same ID
// twice within a single batch window must produce only one UPDATE row
// — Decay accuracy doesn't care about "I touched it 3 times in 5s",
// and dedup keeps the batch shorter.
func TestRunAccessBatcherLoop_DedupWithinBatch(t *testing.T) {
	rec := &flushRecorder{}
	queue := make(chan TouchRef, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runAccessBatcherLoop(ctx, queue, 100, 30*time.Millisecond, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- idRef("x")
	queue <- idRef("x")
	queue <- idRef("x")
	queue <- idRef("y")

	require.Eventually(t, func() bool { return rec.total() == 2 }, 2*time.Second, 5*time.Millisecond)

	cancel()
	<-done

	snap := rec.idsSnapshot()
	require.GreaterOrEqual(t, len(snap), 1)
	assert.ElementsMatch(t, []string{"x", "y"}, snap[0])
}

// TestRunAccessBatcherLoop_DrainsOnContextCancel: a graceful shutdown
// must NOT lose the last partial batch — main.go relies on this so
// the final 5s of access signal survives the SIGTERM.
func TestRunAccessBatcherLoop_DrainsOnContextCancel(t *testing.T) {
	rec := &flushRecorder{}
	queue := make(chan TouchRef, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runAccessBatcherLoop(ctx, queue, 100, 1*time.Hour, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- idRef("last1")
	queue <- idRef("last2")
	time.Sleep(20 * time.Millisecond) // let the IDs reach the goroutine
	cancel()
	<-done

	snap := rec.idsSnapshot()
	require.Len(t, snap, 1, "exactly one final flush on shutdown")
	assert.ElementsMatch(t, []string{"last1", "last2"}, snap[0])
}

// TestRunAccessBatcherLoop_RefRoundTrip is the P0 #5 lock-in: the loop
// must propagate UserID + ProjectID unchanged so the hot-tier flush
// can reconstruct the Redis key. Regression target: someone refactors
// touchQueue back to chan string.
func TestRunAccessBatcherLoop_RefRoundTrip(t *testing.T) {
	rec := &flushRecorder{}
	queue := make(chan TouchRef, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runAccessBatcherLoop(ctx, queue, 2, 1*time.Hour, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- refOf("alice", "p1", "m1")
	queue <- refOf("bob", "p2", "m2")
	require.Eventually(t, func() bool { return rec.total() == 2 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	refs := rec.refsSnapshot()
	require.Len(t, refs, 1)
	require.Len(t, refs[0], 2)
	got := map[string]TouchRef{refs[0][0].ID: refs[0][0], refs[0][1].ID: refs[0][1]}
	assert.Equal(t, TouchRef{UserID: "alice", ProjectID: "p1", ID: "m1"}, got["m1"])
	assert.Equal(t, TouchRef{UserID: "bob", ProjectID: "p2", ID: "m2"}, got["m2"])
}

// TestEnqueueTouches_DropOnFullQueue: the read path MUST NOT block on
// the touch queue. When the queue is full, drops are counted but the
// caller proceeds unblocked.
func TestEnqueueTouches_DropOnFullQueue(t *testing.T) {
	h := &HybridStore{
		logger:     zap.NewNop(),
		touchQueue: make(chan TouchRef, 2), // tiny on purpose
	}
	h.accessOpts = AccessBatcherOptions{}.withDefaults()

	mems := []Memory{
		{ID: "a", UserID: "u", ProjectID: "p"},
		{ID: "b", UserID: "u", ProjectID: "p"},
		{ID: "c", UserID: "u", ProjectID: "p"},
		{ID: "d", UserID: "u", ProjectID: "p"},
	}

	t0 := time.Now()
	h.enqueueTouches(mems)
	elapsed := time.Since(t0)
	assert.Less(t, elapsed, 50*time.Millisecond, "enqueueTouches must be non-blocking even when queue is full")
	assert.Equal(t, 2, len(h.touchQueue), "queue should be exactly full (cap=2)")
}

// TestEnqueueTouches_NilQueueIsNoOp: when EnableAccessBatcher hasn't
// been called the read path should silently skip enqueue — don't blow
// up on nil chan deref.
func TestEnqueueTouches_NilQueueIsNoOp(t *testing.T) {
	h := &HybridStore{logger: zap.NewNop()} // touchQueue == nil
	require.NotPanics(t, func() {
		h.enqueueTouches([]Memory{{ID: "x"}, {ID: "y"}})
	})
}

// TestEnqueueTouches_IgnoresEmptyIDs: defense against producers that
// somehow yield zero-ID memories (e.g. a partially-filled DTO).
func TestEnqueueTouches_IgnoresEmptyIDs(t *testing.T) {
	h := &HybridStore{
		logger:     zap.NewNop(),
		touchQueue: make(chan TouchRef, 4),
	}
	h.accessOpts = AccessBatcherOptions{}.withDefaults()
	h.enqueueTouches([]Memory{{ID: ""}, {ID: "real"}, {ID: ""}})
	assert.Equal(t, 1, len(h.touchQueue))
}

// TestEnqueueTouches_CarriesUserProjectIDs locks in P0 #5: enqueue
// must thread UserID + ProjectID through TouchRef so hot can
// reconstruct keys.
func TestEnqueueTouches_CarriesUserProjectIDs(t *testing.T) {
	h := &HybridStore{
		logger:     zap.NewNop(),
		touchQueue: make(chan TouchRef, 4),
	}
	h.accessOpts = AccessBatcherOptions{}.withDefaults()
	h.enqueueTouches([]Memory{{ID: "m1", UserID: "alice", ProjectID: "p1"}})

	require.Equal(t, 1, len(h.touchQueue))
	got := <-h.touchQueue
	assert.Equal(t, TouchRef{UserID: "alice", ProjectID: "p1", ID: "m1"}, got)
}

// TestRunAccessBatcherLoop_FlushErrorDoesNotKillLoop: an UPDATE error
// (PG hiccup) must NOT take down the batcher — the next flush should
// still attempt to run. This is what keeps Decay accuracy converging
// after transient PG outages.
func TestRunAccessBatcherLoop_FlushErrorDoesNotKillLoop(t *testing.T) {
	rec := &flushRecorder{}
	rec.failNext.Store(true) // first flush errors
	queue := make(chan TouchRef, 16)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runAccessBatcherLoop(ctx, queue, 2, 30*time.Millisecond, rec.flush, zap.NewNop())
		close(done)
	}()

	queue <- idRef("a")
	queue <- idRef("b") // hits size=2, first flush errors
	queue <- idRef("c")
	queue <- idRef("d") // hits size=2 again, second flush succeeds

	require.Eventually(t, func() bool { return rec.total() >= 2 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	snap := rec.idsSnapshot()
	// The errored batch is intentionally not in snapshot (failNext made
	// flush return early). We assert the loop kept running and produced
	// the second batch.
	require.GreaterOrEqual(t, len(snap), 1)
	assert.Equal(t, []string{"c", "d"}, snap[0])
}
