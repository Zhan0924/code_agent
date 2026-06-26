package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Decay-specific helpers — kept here (rather than in the shared
// retrieve_test fixture) to keep the seed values self-documenting:
// each test reads "what does the world look like before decay?" at
// a glance.

func seedMem(t *testing.T, hot *memory.RedisHot, m memory.Memory) {
	t.Helper()
	require.NoError(t, hot.Store(context.Background(), &m))
}

// fetchOne pulls a single memory back via Retrieve(limit=10) and asserts
// it exists. Returns the parsed Memory for further inspection.
func fetchOne(t *testing.T, hot *memory.RedisHot, userID, projectID, id string) memory.Memory {
	t.Helper()
	got, err := hot.Retrieve(context.Background(), userID, projectID, 10)
	require.NoError(t, err)
	for _, m := range got {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("memory %s not found for %s/%s", id, userID, projectID)
	return memory.Memory{}
}

// TestRedisHot_DecayTenants_OnlyTouchesListedTenants is the headline
// P1 #6 assertion: passing tenants=[u1/p1] must NOT decay entries
// belonging to u2/p2, even if they're stale by the same cutoff. The
// pre-fix `memory:*` scan would have decayed both.
func TestRedisHot_DecayTenants_OnlyTouchesListedTenants(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)

	seedMem(t, hot, memory.Memory{ID: "m1", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 1.0, LastAccessedAt: old})
	seedMem(t, hot, memory.Memory{ID: "m2", UserID: "u2", ProjectID: "p2",
		Type: memory.MemoryPreference, Score: 1.0, LastAccessedAt: old})

	// Only u1/p1 in the tenant list. demoteThreshold=0 disables the
	// P1 #8 hot-eviction path for these tests; explicit demote
	// behavior is exercised in TestRedisHot_DecayTenants_DemotesBelowThreshold.
	count, err := hot.DecayTenants(context.Background(),
		[]memory.TenantRef{{UserID: "u1", ProjectID: "p1"}},
		30*24*time.Hour, 0.8, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	got1 := fetchOne(t, hot, "u1", "p1", "m1")
	assert.InDelta(t, 0.8, got1.Score, 0.001, "u1/p1 must be decayed")

	got2 := fetchOne(t, hot, "u2", "p2", "m2")
	assert.InDelta(t, 1.0, got2.Score, 0.001, "u2/p2 must be untouched")
}

// TestRedisHot_DecayTenants_UsesLastAccessedAt locks in the field
// switch: the pre-fix code used UpdatedAt, so a Memory that was
// *written* recently but never *read* would correctly be left alone
// — but a Memory that was *read* recently but written long ago would
// also (incorrectly) be left alone. The new code uses LastAccessedAt,
// which is the proper "stale" signal.
func TestRedisHot_DecayTenants_UsesLastAccessedAt(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-1 * time.Hour)

	// stale-read: hasn't been accessed in 40 days, but UpdatedAt is
	// recent (e.g. ConflictResolver re-merged it). Should decay.
	seedMem(t, hot, memory.Memory{ID: "stale-read", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 1.0,
		LastAccessedAt: old, UpdatedAt: recent})

	// fresh-read: read 1h ago, but UpdatedAt is ancient (rarely
	// re-merged but still in active use). Must NOT decay.
	seedMem(t, hot, memory.Memory{ID: "fresh-read", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 1.0,
		LastAccessedAt: recent, UpdatedAt: old})

	count, err := hot.DecayTenants(context.Background(),
		[]memory.TenantRef{{UserID: "u1", ProjectID: "p1"}},
		30*24*time.Hour, 0.5, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "exactly one of the two should decay")

	gotStale := fetchOne(t, hot, "u1", "p1", "stale-read")
	assert.InDelta(t, 0.5, gotStale.Score, 0.001, "stale by access time → decayed")
	gotFresh := fetchOne(t, hot, "u1", "p1", "fresh-read")
	assert.InDelta(t, 1.0, gotFresh.Score, 0.001, "fresh by access time → untouched")
}

// TestRedisHot_DecayTenants_RespectsScoreFloor: anything at-or-below
// 0.01 is already at the noise floor; further decay would just churn
// Redis writes. Match PGCold.Decay's `score > 0.01` filter.
func TestRedisHot_DecayTenants_RespectsScoreFloor(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)

	seedMem(t, hot, memory.Memory{ID: "at-floor", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 0.01, LastAccessedAt: old})
	seedMem(t, hot, memory.Memory{ID: "below-floor", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 0.005, LastAccessedAt: old})
	seedMem(t, hot, memory.Memory{ID: "above-floor", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 0.5, LastAccessedAt: old})

	count, err := hot.DecayTenants(context.Background(),
		[]memory.TenantRef{{UserID: "u1", ProjectID: "p1"}},
		30*24*time.Hour, 0.5, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "only above-floor should decay")

	gotAbove := fetchOne(t, hot, "u1", "p1", "above-floor")
	assert.InDelta(t, 0.25, gotAbove.Score, 0.001)
	gotAtFloor := fetchOne(t, hot, "u1", "p1", "at-floor")
	assert.InDelta(t, 0.01, gotAtFloor.Score, 0.001, "score==0.01 stays put")
	gotBelow := fetchOne(t, hot, "u1", "p1", "below-floor")
	assert.InDelta(t, 0.005, gotBelow.Score, 0.001, "score<0.01 stays put")
}

// TestRedisHot_DecayTenants_PreservesTTL: KeepTTL is the difference
// between "this entry's score dropped" and "this entry got a free
// fresh 24h lease". The latter would defeat the 24h hot expiry.
//
// We fast-forward miniredis time between seed and decay so the TTL
// window shrinks measurably; a decay that lost KeepTTL would reset
// it back to ~24h and the assertion would catch it.
func TestRedisHot_DecayTenants_PreservesTTL(t *testing.T) {
	hot, mr, cleanup := hotTestClientWithMR(t)
	defer cleanup()

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	mem := memory.Memory{ID: "ttl-check", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 1.0, LastAccessedAt: old}
	seedMem(t, hot, mem)

	key := "memory:u1:p1:ttl-check"
	ttlAfterSeed := mr.TTL(key)
	require.Greater(t, ttlAfterSeed, 23*time.Hour,
		"sanity check: just-stored entry should have ~24h TTL")

	// Fast-forward the in-memory clock by 6h so the TTL is observably
	// shorter than the default 24h. After this, any reset would push
	// it back to ~24h and be obvious.
	mr.FastForward(6 * time.Hour)
	ttlBeforeDecay := mr.TTL(key)
	require.Less(t, ttlBeforeDecay, 19*time.Hour,
		"sanity check: post-fast-forward TTL should be ~18h")

	_, err := hot.DecayTenants(context.Background(),
		[]memory.TenantRef{{UserID: "u1", ProjectID: "p1"}},
		30*24*time.Hour, 0.5, 0)
	require.NoError(t, err)

	ttlAfterDecay := mr.TTL(key)
	assert.Less(t, ttlAfterDecay, 19*time.Hour,
		"TTL must remain shrunken (KeepTTL); got %s — if this is ~24h decay lost the TTL",
		ttlAfterDecay)
}

// TestRedisHot_DecayTenants_SkipsMalformedTenant: a TenantRef with
// empty UserID or ProjectID is a no-op and bumps the "skip" metric.
// Without the guard, scanAll on `memory::p1:*` would match nothing
// (or worse, hit unintended keys with similar prefixes).
func TestRedisHot_DecayTenants_SkipsMalformedTenant(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	seedMem(t, hot, memory.Memory{ID: "m1", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 1.0, LastAccessedAt: old})

	count, err := hot.DecayTenants(context.Background(),
		[]memory.TenantRef{
			{UserID: "", ProjectID: "p1"},  // skip
			{UserID: "u1", ProjectID: ""},  // skip
			{UserID: "u1", ProjectID: "p1"}, // valid
		},
		30*24*time.Hour, 0.5, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "only the valid tenant is processed")

	got := fetchOne(t, hot, "u1", "p1", "m1")
	assert.InDelta(t, 0.5, got.Score, 0.001)
}

// TestRedisHot_DecayTenants_EmptyTenantList: no tenants → no work →
// no error. Important contract for HybridStore.Decay when cold
// returns "nobody is stale right now".
func TestRedisHot_DecayTenants_EmptyTenantList(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	count, err := hot.DecayTenants(context.Background(), nil, 30*24*time.Hour, 0.5, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	count, err = hot.DecayTenants(context.Background(), []memory.TenantRef{},
		30*24*time.Hour, 0.5, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// TestRedisHot_Decay_FallbackUsesLastAccessedAt: the legacy whole-DB
// fallback (used when cold == nil) must also use LastAccessedAt now,
// not UpdatedAt — otherwise pure-hot deployments would still see the
// old "write-not-read" decay bug.
func TestRedisHot_Decay_FallbackUsesLastAccessedAt(t *testing.T) {
	hot, cleanup := hotTestClient(t)
	defer cleanup()

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-1 * time.Hour)

	seedMem(t, hot, memory.Memory{ID: "stale-read", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 1.0,
		LastAccessedAt: old, UpdatedAt: recent})
	seedMem(t, hot, memory.Memory{ID: "fresh-read", UserID: "u2", ProjectID: "p2",
		Type: memory.MemoryPreference, Score: 1.0,
		LastAccessedAt: recent, UpdatedAt: old})

	count, err := hot.Decay(context.Background(), 30*24*time.Hour, 0.5, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// TestRedisHot_DecayTenants_DemotesBelowThreshold is the P1 #8 demote
// assertion: when post-decay score crosses below demoteThreshold, the
// hot copy must be DEL'd (not just SET with reduced score). cold keeps
// the truth — hot just stops caching the low-signal entry.
func TestRedisHot_DecayTenants_DemotesBelowThreshold(t *testing.T) {
	hot, mr, cleanup := hotTestClientWithMR(t)
	defer cleanup()

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)

	// Entry currently at 0.5; decay factor 0.5 → newScore = 0.25.
	// demoteThreshold = 0.3 → cross-below → DEL.
	seedMem(t, hot, memory.Memory{ID: "demote-me", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 0.5, LastAccessedAt: old})

	count, err := hot.DecayTenants(context.Background(),
		[]memory.TenantRef{{UserID: "u1", ProjectID: "p1"}},
		30*24*time.Hour, 0.5, 0.3) // demoteThreshold=0.3
	require.NoError(t, err)
	assert.Equal(t, 1, count, "demote still counts as one affected entry")

	// Key must be GONE from miniredis — not just at reduced score.
	assert.False(t, mr.Exists("memory:u1:p1:demote-me"),
		"demote should DEL the key from hot")
}

// TestRedisHot_DecayTenants_KeepsAboveThreshold: an entry that decays
// but stays above demoteThreshold must NOT be deleted — only SET with
// reduced score.
func TestRedisHot_DecayTenants_KeepsAboveThreshold(t *testing.T) {
	hot, mr, cleanup := hotTestClientWithMR(t)
	defer cleanup()

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)

	// Entry at 0.9; decay factor 0.8 → newScore = 0.72.
	// demoteThreshold = 0.3 → stays above → SET (KeepTTL).
	seedMem(t, hot, memory.Memory{ID: "keep-me", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 0.9, LastAccessedAt: old})

	count, err := hot.DecayTenants(context.Background(),
		[]memory.TenantRef{{UserID: "u1", ProjectID: "p1"}},
		30*24*time.Hour, 0.8, 0.3)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	require.True(t, mr.Exists("memory:u1:p1:keep-me"), "key must survive")
	got := fetchOne(t, hot, "u1", "p1", "keep-me")
	assert.InDelta(t, 0.72, got.Score, 0.001, "score should be 0.9*0.8")
}

// TestRedisHot_DecayTenants_DoesNotDemoteAlreadyBelow: if an entry was
// already below threshold (cold could have decayed it earlier), don't
// keep DEL-ing on each decay tick — that would be wasted work and a
// confusing metric story. We only demote on the *crossing* event.
func TestRedisHot_DecayTenants_DoesNotDemoteAlreadyBelow(t *testing.T) {
	hot, mr, cleanup := hotTestClientWithMR(t)
	defer cleanup()

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)

	// Entry at 0.2 (already below threshold 0.3). Decay would push it
	// to 0.16, still below 0.3 — we keep it as SET, not DEL.
	// Rationale: it's already "below threshold"; the cross-below event
	// already happened in a previous tick or it was never above
	// threshold. DELing now isn't wrong but spams metrics on every
	// decay round; SET is fine since cold holds truth.
	seedMem(t, hot, memory.Memory{ID: "already-low", UserID: "u1", ProjectID: "p1",
		Type: memory.MemoryPreference, Score: 0.2, LastAccessedAt: old})

	_, err := hot.DecayTenants(context.Background(),
		[]memory.TenantRef{{UserID: "u1", ProjectID: "p1"}},
		30*24*time.Hour, 0.8, 0.3)
	require.NoError(t, err)

	assert.True(t, mr.Exists("memory:u1:p1:already-low"),
		"already-below entries are not re-demoted on subsequent decays")
}
