package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisHot_TouchBatch_BumpsAccessCountAndLastAccessedAt is the
// headline P0 #5 assertion: after the read-path batcher fires
// TouchBatch, hot's JSON for each touched key must show AccessCount+1
// and a fresh LastAccessedAt — otherwise the next Retrieve would
// re-order memories using stale access metadata.
func TestRedisHot_TouchBatch_BumpsAccessCountAndLastAccessedAt(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	seed := []memory.Memory{
		{ID: "m1", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference, Content: "a", Score: 1, AccessCount: 3, LastAccessedAt: old},
		{ID: "m2", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference, Content: "b", Score: 1, AccessCount: 7, LastAccessedAt: old},
		{ID: "m3", UserID: "u2", ProjectID: "p9", Type: memory.MemoryPreference, Content: "c", Score: 1, AccessCount: 0, LastAccessedAt: old},
	}
	for i := range seed {
		require.NoError(t, hot.Store(ctx, &seed[i]))
	}

	before := time.Now().UTC()
	err := hot.TouchBatch(ctx, []memory.TouchRef{
		{UserID: "u1", ProjectID: "p1", ID: "m1"},
		{UserID: "u1", ProjectID: "p1", ID: "m2"},
		{UserID: "u2", ProjectID: "p9", ID: "m3"},
	})
	require.NoError(t, err)

	// Re-fetch via Retrieve (only u1/p1 should appear, we read u2 separately).
	gotU1, err := hot.Retrieve(ctx, "u1", "p1", 10)
	require.NoError(t, err)
	require.Len(t, gotU1, 2)

	byID := map[string]memory.Memory{}
	for _, m := range gotU1 {
		byID[m.ID] = m
	}
	assert.Equal(t, 4, byID["m1"].AccessCount, "m1 AccessCount should be 3+1")
	assert.Equal(t, 8, byID["m2"].AccessCount, "m2 AccessCount should be 7+1")
	assert.True(t, !byID["m1"].LastAccessedAt.Before(before),
		"LastAccessedAt must be refreshed to >= before-mark")
	assert.True(t, !byID["m2"].LastAccessedAt.Before(before),
		"LastAccessedAt must be refreshed to >= before-mark")

	// Cross-tenant key still updated correctly.
	gotU2, err := hot.Retrieve(ctx, "u2", "p9", 10)
	require.NoError(t, err)
	require.Len(t, gotU2, 1)
	assert.Equal(t, 1, gotU2[0].AccessCount)
}

// TestRedisHot_TouchBatch_SkipsMissingKeys: TouchBatch is "refresh if
// cached", not "cache from cold" — a ref whose key doesn't exist in
// hot must be silently skipped, not error out the whole batch.
func TestRedisHot_TouchBatch_SkipsMissingKeys(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	seed := memory.Memory{
		ID: "present", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Content: "x", Score: 1,
		AccessCount: 0, LastAccessedAt: time.Now().UTC(),
	}
	require.NoError(t, hot.Store(ctx, &seed))

	err := hot.TouchBatch(ctx, []memory.TouchRef{
		{UserID: "u1", ProjectID: "p1", ID: "present"},
		{UserID: "u1", ProjectID: "p1", ID: "ghost-1"}, // not in hot
		{UserID: "u1", ProjectID: "p1", ID: "ghost-2"}, // not in hot
	})
	require.NoError(t, err, "missing keys must not surface as errors")

	got, err := hot.Retrieve(ctx, "u1", "p1", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 1, got[0].AccessCount, "present key still bumped exactly once")
}

// TestRedisHot_TouchBatch_EmptyRefsNoOp: contract for the batcher's
// graceful-shutdown path — flush([]) MUST be cheap and non-erroring.
func TestRedisHot_TouchBatch_EmptyRefsNoOp(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	require.NoError(t, hot.TouchBatch(ctx, nil))
	require.NoError(t, hot.TouchBatch(ctx, []memory.TouchRef{}))
}

// TestRedisHot_DeleteBatch_DropsAllRefs is the P1 #7 hot-side dedup
// contract: when HybridStore's dedup branch removes N-1 duplicates from
// cold, the same N-1 entries must disappear from hot, otherwise the
// next Retrieve would still surface them until TTL expiry (24h).
func TestRedisHot_DeleteBatch_DropsAllRefs(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	seed := []memory.Memory{
		{ID: "keep", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference, Content: "anchor", Score: 1, LastAccessedAt: now},
		{ID: "dup1", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference, Content: "dup1", Score: 1, LastAccessedAt: now},
		{ID: "dup2", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference, Content: "dup2", Score: 1, LastAccessedAt: now},
		// cross-tenant must NOT be touched by deleting u1/p1 refs.
		{ID: "other", UserID: "u2", ProjectID: "p2", Type: memory.MemoryPreference, Content: "other", Score: 1, LastAccessedAt: now},
	}
	for i := range seed {
		require.NoError(t, hot.Store(ctx, &seed[i]))
	}

	err := hot.DeleteBatch(ctx, []memory.TouchRef{
		{UserID: "u1", ProjectID: "p1", ID: "dup1"},
		{UserID: "u1", ProjectID: "p1", ID: "dup2"},
	})
	require.NoError(t, err)

	gotU1, err := hot.Retrieve(ctx, "u1", "p1", 10)
	require.NoError(t, err)
	require.Len(t, gotU1, 1)
	assert.Equal(t, "keep", gotU1[0].ID)

	gotU2, err := hot.Retrieve(ctx, "u2", "p2", 10)
	require.NoError(t, err)
	require.Len(t, gotU2, 1, "cross-tenant key must survive")
	assert.Equal(t, "other", gotU2[0].ID)
}

// TestRedisHot_DeleteBatch_EmptyAndMissingNoOp: empty input is a
// no-op; missing keys are tolerated (DEL returns 0 affected, but no
// error). Important contract for HybridStore's dedup branch when hot
// has already expired the duplicate via TTL.
func TestRedisHot_DeleteBatch_EmptyAndMissingNoOp(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	require.NoError(t, hot.DeleteBatch(ctx, nil))
	require.NoError(t, hot.DeleteBatch(ctx, []memory.TouchRef{}))
	require.NoError(t, hot.DeleteBatch(ctx, []memory.TouchRef{
		{UserID: "ghost", ProjectID: "p", ID: "never-existed"},
	}))
}
