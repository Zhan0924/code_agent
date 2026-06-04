package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestServer_Drain_WaitsForInflight pins the contract for PR 6: Drain blocks
// until all goroutines registered via trackInflight finish. The fix targets
// the case where SIGTERM lands while handleChat's detached goroutine is mid
// ReAct loop — without Drain, the process exits and the session never
// receives the final answer.
func TestServer_Drain_WaitsForInflight(t *testing.T) {
	s := &Server{logger: zap.NewNop()}

	var finished int32
	s.trackInflight(func() {
		time.Sleep(150 * time.Millisecond)
		atomic.StoreInt32(&finished, 1)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("Drain returned error: %v", err)
	}
	if atomic.LoadInt32(&finished) != 1 {
		t.Fatal("Drain returned before inflight goroutine completed")
	}
}

// TestServer_Drain_TimeoutReturnsErr verifies the ctx-bounded behaviour: when
// the drain context expires before the inflight work finishes, Drain must
// return ctx.Err() rather than block forever. main.go relies on this to log
// a warning and exit when an agent is genuinely stuck.
func TestServer_Drain_TimeoutReturnsErr(t *testing.T) {
	s := &Server{logger: zap.NewNop()}

	s.trackInflight(func() {
		time.Sleep(2 * time.Second)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := s.Drain(ctx)
	if err == nil {
		t.Fatal("expected Drain to return context.DeadlineExceeded, got nil")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

// TestServer_Drain_NoInflightReturnsImmediately exercises the trivial path —
// no work registered, Drain returns instantly. Guards against any future
// refactor that introduces a sentinel goroutine the counter never sees.
func TestServer_Drain_NoInflightReturnsImmediately(t *testing.T) {
	s := &Server{logger: zap.NewNop()}

	start := time.Now()
	if err := s.Drain(context.Background()); err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("expected near-immediate return, took %v", elapsed)
	}
}

// TestServer_TrackInflight_NilFnIsNoop pins the defensive guard so a future
// refactor that passes nil (e.g., a disabled feature path) doesn't panic.
func TestServer_TrackInflight_NilFnIsNoop(t *testing.T) {
	s := &Server{logger: zap.NewNop()}
	s.trackInflight(nil)
	if err := s.Drain(context.Background()); err != nil {
		t.Fatalf("unexpected drain error: %v", err)
	}
}
