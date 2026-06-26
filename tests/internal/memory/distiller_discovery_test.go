package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptrTime returns a pointer to "now" — used to mark fake episodic
// entries as already-distilled in fakeDistillerStore.mems literals.
func ptrTime() *time.Time {
	t := time.Now()
	return &t
}

// TestListActiveDistillTenants_ThresholdAndOrder verifies the GROUP BY +
// HAVING + ORDER BY semantics that the PG implementation also relies on:
//   - tenants below minEpisodic are filtered out (no LLM waste)
//   - results are sorted by undistilled count DESC (most-starved first)
//   - already-distilled episodes don't count
//   - limit truncates the tail
func TestListActiveDistillTenants_ThresholdAndOrder(t *testing.T) {
	store := &fakeDistillerStore{mems: []memory.Memory{
		// tenant A: 4 undistilled (above threshold)
		{ID: "a1", Type: memory.MemoryTypeEpisodic, UserID: "uA", ProjectID: "p1"},
		{ID: "a2", Type: memory.MemoryTypeEpisodic, UserID: "uA", ProjectID: "p1"},
		{ID: "a3", Type: memory.MemoryTypeEpisodic, UserID: "uA", ProjectID: "p1"},
		{ID: "a4", Type: memory.MemoryTypeEpisodic, UserID: "uA", ProjectID: "p1"},
		// tenant B: 2 undistilled (above threshold)
		{ID: "b1", Type: memory.MemoryTypeEpisodic, UserID: "uB", ProjectID: "p1"},
		{ID: "b2", Type: memory.MemoryTypeEpisodic, UserID: "uB", ProjectID: "p1"},
		// tenant C: 1 undistilled (below threshold)
		{ID: "c1", Type: memory.MemoryTypeEpisodic, UserID: "uC", ProjectID: "p1"},
		// tenant D: 3 episodes but all already-distilled
		{ID: "d1", Type: memory.MemoryTypeEpisodic, UserID: "uD", ProjectID: "p1", DistilledAt: ptrTime()},
		{ID: "d2", Type: memory.MemoryTypeEpisodic, UserID: "uD", ProjectID: "p1", DistilledAt: ptrTime()},
		{ID: "d3", Type: memory.MemoryTypeEpisodic, UserID: "uD", ProjectID: "p1", DistilledAt: ptrTime()},
		// non-episodic should be ignored
		{ID: "k1", Type: memory.MemoryKnowledge, UserID: "uA", ProjectID: "p1"},
	}}

	tenants, err := store.ListActiveDistillTenants(context.Background(), 2, 10)
	require.NoError(t, err)
	require.Len(t, tenants, 2, "C should be filtered (below threshold); D should be filtered (all distilled)")

	assert.Equal(t, "uA", tenants[0].UserID, "highest-count tenant first")
	assert.Equal(t, 4, tenants[0].Count)
	assert.Equal(t, "uB", tenants[1].UserID)
	assert.Equal(t, 2, tenants[1].Count)
}

// TestListActiveDistillTenants_Cap exercises the limit knob — exactly the
// MaxTenantsPerTick guard. A backlog of N tenants must not blow the LLM
// rate limit; the scheduler caps fan-out, the discovery layer enforces it
// at the SQL/query layer.
func TestListActiveDistillTenants_Cap(t *testing.T) {
	mems := make([]memory.Memory, 0, 30)
	for i := 0; i < 10; i++ {
		userID := "u" + string(rune('A'+i))
		// each tenant gets 3 undistilled episodes (above threshold)
		for j := 0; j < 3; j++ {
			mems = append(mems, memory.Memory{
				ID:        userID + "-" + string(rune('0'+j)),
				Type:      memory.MemoryTypeEpisodic,
				UserID:    userID,
				ProjectID: "p1",
			})
		}
	}
	store := &fakeDistillerStore{mems: mems}

	tenants, err := store.ListActiveDistillTenants(context.Background(), 2, 3)
	require.NoError(t, err)
	assert.Len(t, tenants, 3, "limit=3 must truncate fan-out")
}

// TestDistill_AcrossDiscoveredTenants is the integration check that
// the scheduler's actual data path works end-to-end: discover three
// tenants from the store, distill each, and verify each produced a
// distinct semantic memory keyed by (user, project).
func TestDistill_AcrossDiscoveredTenants(t *testing.T) {
	store := &fakeDistillerStore{mems: []memory.Memory{
		{ID: "e1", Type: memory.MemoryTypeEpisodic, Content: "alice ep 1", Score: 1, UserID: "alice", ProjectID: "p1"},
		{ID: "e2", Type: memory.MemoryTypeEpisodic, Content: "alice ep 2", Score: 1, UserID: "alice", ProjectID: "p1"},
		{ID: "e3", Type: memory.MemoryTypeEpisodic, Content: "bob ep 1", Score: 1, UserID: "bob", ProjectID: "p1"},
		{ID: "e4", Type: memory.MemoryTypeEpisodic, Content: "bob ep 2", Score: 1, UserID: "bob", ProjectID: "p1"},
	}}

	distiller := memory.NewDistiller(store, &MockLLM{}, nil, nil, memory.DistillerOptions{
		MinEpisodicToTrigger: 2,
	})

	tenants, err := store.ListActiveDistillTenants(context.Background(), 2, 10)
	require.NoError(t, err)
	require.Len(t, tenants, 2)

	for _, ten := range tenants {
		n, err := distiller.Distill(context.Background(), ten.UserID, ten.ProjectID)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
	}

	// Both tenants now have a semantic memory; episodes are all marked.
	var aliceSem, bobSem int
	for _, m := range store.mems {
		if m.Type == memory.MemoryTypeSemantic && m.UserID == "alice" {
			aliceSem++
		}
		if m.Type == memory.MemoryTypeSemantic && m.UserID == "bob" {
			bobSem++
		}
	}
	assert.Equal(t, 1, aliceSem)
	assert.Equal(t, 1, bobSem)

	// Re-discovery should now find nothing — every episode is marked.
	tenants2, err := store.ListActiveDistillTenants(context.Background(), 2, 10)
	require.NoError(t, err)
	assert.Empty(t, tenants2, "after distillation, all undistilled counts must drop to 0")
}
