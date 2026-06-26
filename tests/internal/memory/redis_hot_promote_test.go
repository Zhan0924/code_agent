package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisHot_PromoteBatch_WritesAllToHotWithTTL is the headline
// P1 #8 promote assertion: a batch of memories handed off from the
// hybrid read path's back-fill goroutine must all end up in hot with
// the configured TTL (24h by default), in a single round trip.
func TestRedisHot_PromoteBatch_WritesAllToHotWithTTL(t *testing.T) {
	hot, mr, cleanup := hotTestClientWithMR(t)
	defer cleanup()

	mems := []memory.Memory{
		{ID: "p1", UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryPreference, Score: 0.9},
		{ID: "p2", UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryDecision, Score: 0.95},
		{ID: "p3", UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryKnowledge, Score: 0.8},
	}
	require.NoError(t, hot.PromoteBatch(context.Background(), mems))

	for _, m := range mems {
		key := "memory:u1:p1:" + m.ID
		assert.True(t, mr.Exists(key), "promoted key %s must exist in hot", key)
		// TTL must be > 0 and roughly the configured 24h. Miniredis TTL
		// is exact when set explicitly — we just assert the lifetime
		// is long-ish (anything >12h means it was set with a TTL, not
		// KEEPTTL on a missing key which would have been -1).
		ttl := mr.TTL(key)
		assert.True(t, ttl > 12*time.Hour,
			"promoted key %s should carry the hot TTL, got %s", key, ttl)
	}
}

// TestRedisHot_PromoteBatch_EmptyAndInvalidNoOp: empty input returns
// nil and zero side-effects. Entries missing required fields
// (ID/UserID/ProjectID) are silently skipped — Promote is a soft
// best-effort back-fill, so we don't want one bad record to fail the
// whole batch.
func TestRedisHot_PromoteBatch_EmptyAndInvalidNoOp(t *testing.T) {
	hot, mr, cleanup := hotTestClientWithMR(t)
	defer cleanup()

	// Empty batch.
	require.NoError(t, hot.PromoteBatch(context.Background(), nil))
	require.NoError(t, hot.PromoteBatch(context.Background(), []memory.Memory{}))
	assert.Empty(t, mr.Keys(), "no keys should be written for empty batches")

	// All entries invalid → still no-op success.
	bad := []memory.Memory{
		{ID: "", UserID: "u1", ProjectID: "p1"},
		{ID: "x", UserID: "", ProjectID: "p1"},
		{ID: "y", UserID: "u1", ProjectID: ""},
	}
	require.NoError(t, hot.PromoteBatch(context.Background(), bad))
	assert.Empty(t, mr.Keys(),
		"entries missing required fields must be skipped, not error out")
}

// TestRedisHot_PromoteBatch_OverwritesExisting: if a key already exists
// in hot (e.g. another goroutine wrote it), Promote SHOULD overwrite —
// we're trying to converge hot to the most-recent cold snapshot. SET
// without NX is the right semantic.
func TestRedisHot_PromoteBatch_OverwritesExisting(t *testing.T) {
	hot, _, cleanup := hotTestClientWithMR(t)
	defer cleanup()

	// First write: score 0.5.
	require.NoError(t, hot.PromoteBatch(context.Background(),
		[]memory.Memory{{ID: "p1", UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryPreference, Score: 0.5}}))

	// Overwrite: score 0.95 (cold's authoritative value).
	require.NoError(t, hot.PromoteBatch(context.Background(),
		[]memory.Memory{{ID: "p1", UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryPreference, Score: 0.95}}))

	got := fetchOne(t, hot, "u1", "p1", "p1")
	assert.InDelta(t, 0.95, got.Score, 0.001,
		"promote should overwrite, not preserve stale hot copy")
}

// TestRedisHot_PromoteBatch_MixedValidAndInvalid: valid entries are
// promoted, invalid ones are skipped — partial success is the contract.
func TestRedisHot_PromoteBatch_MixedValidAndInvalid(t *testing.T) {
	hot, mr, cleanup := hotTestClientWithMR(t)
	defer cleanup()

	mix := []memory.Memory{
		{ID: "good", UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryDecision, Score: 0.9},
		{ID: "", UserID: "u1", ProjectID: "p1"}, // skip
		{ID: "good2", UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryDecision, Score: 0.85},
	}
	require.NoError(t, hot.PromoteBatch(context.Background(), mix))

	assert.True(t, mr.Exists("memory:u1:p1:good"))
	assert.True(t, mr.Exists("memory:u1:p1:good2"))
	assert.Len(t, mr.Keys(), 2, "exactly the two valid entries should land")
}
