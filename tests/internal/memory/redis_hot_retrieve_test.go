package memory_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// hotTestClient spins up an in-process miniredis and returns a configured
// RedisHot plus a cleanup func. Keeps tests independent of localhost:6379.
func hotTestClient(t *testing.T) (*memory.RedisHot, func()) {
	t.Helper()
	hot, _, cleanup := hotTestClientWithMR(t)
	return hot, cleanup
}

// hotTestClientWithMR exposes the miniredis instance for tests that
// need to introspect Redis state directly (e.g. TTL assertions for
// KeepTTL behavior in decay tests). All other callers should prefer
// hotTestClient.
func hotTestClientWithMR(t *testing.T) (*memory.RedisHot, *miniredis.Miniredis, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	hot := memory.NewRedisHot(rdb, zap.NewNop())
	return hot, mr, func() {
		_ = rdb.Close()
		mr.Close()
	}
}

// TestRedisHot_Retrieve_OrdersByLastAccessedAt is the headline P0 #2
// assertion: the prior implementation sorted Redis keys by UUID
// lexicographical order (random), now we sort by Memory.LastAccessedAt
// DESC so callers actually get the "most recently accessed N".
func TestRedisHot_Retrieve_OrdersByLastAccessedAt(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Use IDs that intentionally invert the time order. If the old
	// "sort by key" code path resurfaced, this would return
	// {old-a, mid-m, new-z} and the assertion below would catch it.
	memories := []memory.Memory{
		{ID: "new-z", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference,
			Content: "newest", Score: 1.0, LastAccessedAt: now},
		{ID: "mid-m", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference,
			Content: "middle", Score: 1.0, LastAccessedAt: now.Add(-1 * time.Hour)},
		{ID: "old-a", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference,
			Content: "oldest", Score: 1.0, LastAccessedAt: now.Add(-2 * time.Hour)},
	}
	for i := range memories {
		require.NoError(t, hot.Store(ctx, &memories[i]))
	}

	got, err := hot.Retrieve(ctx, "u1", "p1", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "new-z", got[0].ID, "most recent must be first")
	assert.Equal(t, "mid-m", got[1].ID, "second most recent must be second")
}

// TestRedisHot_Retrieve_FiltersEpisodic confirms the P0 #1 invariant
// (episodic entries are Distiller fuel, not actionable) still holds
// after the sort rewrite.
func TestRedisHot_Retrieve_FiltersEpisodic(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Put a newest episodic that would otherwise hijack position 0.
	require.NoError(t, hot.Store(ctx, &memory.Memory{
		ID: "epi", UserID: "u1", ProjectID: "p1", Type: memory.MemoryTypeEpisodic,
		Content: "raw log", Score: 1.0, LastAccessedAt: now,
	}))
	require.NoError(t, hot.Store(ctx, &memory.Memory{
		ID: "pref", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference,
		Content: "real preference", Score: 1.0, LastAccessedAt: now.Add(-1 * time.Hour),
	}))

	got, err := hot.Retrieve(ctx, "u1", "p1", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "pref", got[0].ID)
}

// TestRedisHot_Retrieve_StableTieBreak: equal LastAccessedAt should
// fall back to ID-ascending order so the result is deterministic
// (matters for snapshot-style integration tests).
func TestRedisHot_Retrieve_StableTieBreak(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	ts := time.Now().UTC().Truncate(time.Second)

	for _, id := range []string{"c", "a", "b"} {
		require.NoError(t, hot.Store(ctx, &memory.Memory{
			ID: id, UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference,
			Content: id, Score: 1.0, LastAccessedAt: ts,
		}))
	}

	got, err := hot.Retrieve(ctx, "u1", "p1", 10)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"a", "b", "c"}, []string{got[0].ID, got[1].ID, got[2].ID})
}

// TestRedisHot_RetrieveByQuery_TieBreakByLastAccessedAt locks in the
// new tie-breaker on retrieveByQueryFiltered: at equal cosine
// similarity, more-recently accessed wins.
func TestRedisHot_RetrieveByQuery_TieBreakByLastAccessedAt(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	// Identical embedding → identical cosine similarity for every entry,
	// so tie-break has to drive ordering.
	emb := []float32{1, 0, 0}

	require.NoError(t, hot.Store(ctx, &memory.Memory{
		ID: "older", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference,
		Content: "older", Score: 1.0, Embedding: emb,
		LastAccessedAt: now.Add(-2 * time.Hour),
	}))
	require.NoError(t, hot.Store(ctx, &memory.Memory{
		ID: "newer", UserID: "u1", ProjectID: "p1", Type: memory.MemoryPreference,
		Content: "newer", Score: 1.0, Embedding: emb,
		LastAccessedAt: now,
	}))

	got, err := hot.RetrieveByQuery(ctx, "u1", "p1", emb, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "newer", got[0].ID, "tie at cosine sim → newest LastAccessedAt wins")
}

// ----------------------------------------------------------------------
// P1 #10 — configurable hot SCAN ceiling
// ----------------------------------------------------------------------

// TestRedisHot_ScanLimit_DefaultsTo200 locks in the documented default
// so a future refactor that drops `defaultHotScanLimit` from the
// constructor doesn't silently reset every deployment to 0 (which
// scanAll would interpret as "use default again" — but it would still
// be a regression to lose the constant).
func TestRedisHot_ScanLimit_DefaultsTo200(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()
	assert.Equal(t, 200, hot.ScanLimit(),
		"NewRedisHot must seed scanLimit to defaultHotScanLimit (200)")
}

// TestRedisHot_SetScanLimit_Clamped covers the operator-facing knob.
// Values outside [50, 2000] are clamped to the boundary; 0 / negative
// reset to the default. The doc comment promises these exact bounds so
// any future tightening must update both the test and the constants.
func TestRedisHot_SetScanLimit_Clamped(t *testing.T) {
	tests := []struct {
		input, want int
		reason      string
	}{
		{0, 200, "zero resets to default"},
		{-100, 200, "negative resets to default"},
		{1, 50, "below floor → clamped to 50"},
		{49, 50, "just below floor → clamped to 50"},
		{50, 50, "at floor → preserved"},
		{500, 500, "in-band value preserved"},
		{2000, 2000, "at ceiling → preserved"},
		{5000, 2000, "above ceiling → clamped to 2000"},
	}

	for _, tt := range tests {
		hot, cleanup := hotTestClient(t)
		hot.SetScanLimit(tt.input)
		got := hot.ScanLimit()
		if got != tt.want {
			t.Errorf("SetScanLimit(%d): got %d, want %d (%s)",
				tt.input, got, tt.want, tt.reason)
		}
		cleanup()
	}
}

// TestRedisHot_RetrieveByQuery_CallerLimitGrowsScanBudget is the
// headline P1 #10 regression target. Before the fix, RetrieveByQuery
// silently capped the SCAN at the const 200 even when HybridStore's
// RetrieveByType passed overFetch*2 = 300+. Now the caller's limit
// raises the effective cap up to maxHotScanLimit.
//
// We seed 250 entries (above default 200, below caller limit 500), all
// with identical embedding so ranking is by LastAccessedAt. With the
// fix, all 250 are SCAN-ed and the result respects caller limit. Pre-
// fix, only 200 would make it past scanAll.
func TestRedisHot_RetrieveByQuery_CallerLimitGrowsScanBudget(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	emb := []float32{1, 0, 0}

	const seed = 250
	for i := 0; i < seed; i++ {
		require.NoError(t, hot.Store(ctx, &memory.Memory{
			ID: fmt.Sprintf("m-%04d", i), UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryPreference, Content: "x", Score: 1.0,
			Embedding: emb, LastAccessedAt: now.Add(-time.Duration(i) * time.Minute),
		}))
	}

	// Default scanLimit=200; caller asks for 500 → effective cap should
	// rise to 500 (still under maxHotScanLimit=2000), so all 250 entries
	// are returned.
	got, err := hot.RetrieveByQuery(ctx, "u1", "p1", emb, 500)
	require.NoError(t, err)
	assert.Equal(t, seed, len(got),
		"caller limit > scanLimit must grow the effective scan cap; pre-fix this would have returned 200")
}

// TestRedisHot_RetrieveByQuery_HitsMaxHotScanLimitCeiling guards the
// upper end: even when a caller passes an absurd limit (say 10_000),
// the scan budget can never exceed maxHotScanLimit=2000 — otherwise a
// hostile caller could pin a Redis worker for >100ms.
func TestRedisHot_RetrieveByQuery_HitsMaxHotScanLimitCeiling(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	ctx := context.Background()
	emb := []float32{1, 0, 0}

	// Seed 2500 entries — above maxHotScanLimit=2000.
	const seed = 2500
	for i := 0; i < seed; i++ {
		require.NoError(t, hot.Store(ctx, &memory.Memory{
			ID: fmt.Sprintf("m-%04d", i), UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryPreference, Content: "x", Score: 1.0,
			Embedding: emb, LastAccessedAt: time.Now().UTC(),
		}))
	}

	// Caller asks for 10000 → effective cap clamped to 2000 → we
	// receive at most 2000 candidates (after sort), then trimmed to
	// caller's limit of 10000 → 2000 final.
	got, err := hot.RetrieveByQuery(ctx, "u1", "p1", emb, 10_000)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(got), 2000,
		"effective cap must clamp at maxHotScanLimit even for adversarial limits")
	assert.Greater(t, len(got), 1500,
		"sanity: we should still get most of the data, just not all 2500")
}

// TestRedisHot_ScanLimit_RespectsCustomFloor ensures SetScanLimit
// raises the steady-state budget across read paths. After
// SetScanLimit(1000), Retrieve and RetrieveByQuery should both scan up
// to 1000 keys even when the caller passes no limit hint.
func TestRedisHot_ScanLimit_RespectsCustomFloor(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()
	hot.SetScanLimit(1000)

	ctx := context.Background()
	now := time.Now().UTC()

	// Seed 600 entries — above default 200, below new scanLimit 1000.
	for i := 0; i < 600; i++ {
		require.NoError(t, hot.Store(ctx, &memory.Memory{
			ID: fmt.Sprintf("m-%04d", i), UserID: "u1", ProjectID: "p1",
			Type: memory.MemoryPreference, Content: "x", Score: 1.0,
			LastAccessedAt: now.Add(-time.Duration(i) * time.Minute),
		}))
	}

	// limit=0 → caller has no hint; scan budget = r.scanLimit = 1000.
	// All 600 should be returned (no truncation).
	got, err := hot.Retrieve(ctx, "u1", "p1", 0)
	require.NoError(t, err)
	assert.Equal(t, 600, len(got),
		"raised scanLimit should let Retrieve see all 600 entries (was capped at 200 pre-fix)")
}
