package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/models"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func newTestStreamCache(t *testing.T) (*StreamCache, *miniredis.Miniredis, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	sc := NewStreamCache(rdb, zap.NewNop())
	if sc == nil {
		t.Fatal("NewStreamCache returned nil with non-nil rdb")
	}
	cleanup := func() {
		_ = rdb.Close()
		mr.Close()
	}
	return sc, mr, cleanup
}

func TestStreamCache_MarkRunningStatus(t *testing.T) {
	sc, _, cleanup := newTestStreamCache(t)
	defer cleanup()
	ctx := context.Background()

	if _, running := sc.Status(ctx, "s1"); running {
		t.Fatal("fresh session should not be running")
	}
	sc.MarkRunning(ctx, "s1", "task-A")
	taskID, running := sc.Status(ctx, "s1")
	if !running || taskID != "task-A" {
		t.Fatalf("after MarkRunning: got running=%v task=%q", running, taskID)
	}
	sc.MarkDone(ctx, "s1")
	if _, running := sc.Status(ctx, "s1"); running {
		t.Fatal("after MarkDone session should not be running")
	}
}

func TestStreamCache_ClearAllRunning(t *testing.T) {
	sc, _, cleanup := newTestStreamCache(t)
	defer cleanup()
	ctx := context.Background()

	// Seed 3 stale running flags (simulating a previous process that died
	// before MarkDone).
	for _, sid := range []string{"s1", "s2", "s3"} {
		sc.MarkRunning(ctx, sid, "task-"+sid)
	}
	for _, sid := range []string{"s1", "s2", "s3"} {
		if _, running := sc.Status(ctx, sid); !running {
			t.Fatalf("seed: %s expected running", sid)
		}
	}

	cleared, err := sc.ClearAllRunning(ctx)
	if err != nil {
		t.Fatalf("ClearAllRunning: %v", err)
	}
	if cleared != 3 {
		t.Fatalf("expected cleared=3, got %d", cleared)
	}
	for _, sid := range []string{"s1", "s2", "s3"} {
		if _, running := sc.Status(ctx, sid); running {
			t.Fatalf("after clear: %s should not be running", sid)
		}
	}

	// Second call on an empty namespace is a no-op (count=0, no error).
	cleared, err = sc.ClearAllRunning(ctx)
	if err != nil {
		t.Fatalf("second ClearAllRunning: %v", err)
	}
	if cleared != 0 {
		t.Fatalf("second call expected 0, got %d", cleared)
	}
}

func TestStreamCache_ClearAllRunning_PreservesEventsStream(t *testing.T) {
	// Critical invariant: clearing status keys must NOT touch the events
	// stream — refreshed clients still need history (just without follow).
	sc, mr, cleanup := newTestStreamCache(t)
	defer cleanup()
	ctx := context.Background()

	sc.MarkRunning(ctx, "sX", "task-X")
	// Manually push one event into the events stream via the same code path
	// so we know it survives.
	sc.Append(ctx, "sX", models.ReactStreamEvent{Type: "step_start", Content: "step 1"})

	if !mr.Exists(streamEventsKeyPrefix + "sX") {
		t.Fatal("events stream missing before clear")
	}

	if _, err := sc.ClearAllRunning(ctx); err != nil {
		t.Fatalf("ClearAllRunning: %v", err)
	}

	if mr.Exists(streamStatusKeyPrefix + "sX") {
		t.Fatal("status key should be gone")
	}
	if !mr.Exists(streamEventsKeyPrefix + "sX") {
		t.Fatal("events stream should be preserved after ClearAllRunning")
	}
}

func TestStreamCache_NilSafe(t *testing.T) {
	var sc *StreamCache
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if n, err := sc.ClearAllRunning(ctx); n != 0 || err != nil {
		t.Fatalf("nil-safe ClearAllRunning: n=%d err=%v", n, err)
	}
	if n := sc.EventCount(ctx, "any"); n != 0 {
		t.Fatalf("nil-safe EventCount: n=%d", n)
	}
}

func TestStreamCache_EventCount(t *testing.T) {
	sc, _, cleanup := newTestStreamCache(t)
	defer cleanup()
	ctx := context.Background()

	// 空 stream → 0
	if n := sc.EventCount(ctx, "s1"); n != 0 {
		t.Fatalf("empty stream: expected 0, got %d", n)
	}

	// Append 3 条 → 3
	for i := range 3 {
		sc.Append(ctx, "s1", models.ReactStreamEvent{
			Type: "step_start", Step: i + 1,
		})
	}
	if n := sc.EventCount(ctx, "s1"); n != 3 {
		t.Fatalf("after 3 appends: expected 3, got %d", n)
	}

	// 关键不变量：MarkDone 不会清掉 events stream
	// （events 的 1h done-TTL 由 markDone 自己设置，本测试用 miniredis 不验证 TTL 时序）
	sc.MarkRunning(ctx, "s1", "task-A")
	sc.MarkDone(ctx, "s1")
	if n := sc.EventCount(ctx, "s1"); n != 3 {
		t.Fatalf("after MarkDone: events still expected 3, got %d", n)
	}
}
