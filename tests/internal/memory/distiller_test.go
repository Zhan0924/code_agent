package memory_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// fakeDistillerStore is a minimal in-memory implementation of
// memory.DistillerStore, used here so we don't need Redis + Postgres just
// to exercise the Distiller's control flow.
//
// ListByType(episodic, ...) intentionally hides entries with a non-nil
// DistilledAt to mirror the contract of PGCold.ListEpisodicUndistilled —
// otherwise the test couldn't distinguish "distill skipped because
// nothing new" from "distill ran on stale data".
type fakeDistillerStore struct {
	mu   sync.Mutex
	mems []memory.Memory
}

func (f *fakeDistillerStore) ListByType(_ context.Context, userID, projectID string, t memory.MemoryType, limit int) ([]memory.Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]memory.Memory, 0, limit)
	for _, m := range f.mems {
		if m.UserID != userID || m.ProjectID != projectID || m.Type != t {
			continue
		}
		if t == memory.MemoryTypeEpisodic && m.DistilledAt != nil {
			continue
		}
		out = append(out, m)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeDistillerStore) Store(_ context.Context, m *memory.Memory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mems = append(f.mems, *m)
	return nil
}

func (f *fakeDistillerStore) MarkDistilled(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	for i := range f.mems {
		if _, ok := idSet[f.mems[i].ID]; ok && f.mems[i].DistilledAt == nil {
			t := now
			f.mems[i].DistilledAt = &t
		}
	}
	return nil
}

// ListActiveDistillTenants mirrors PGCold's GROUP BY semantics: count
// undistilled episodic per (user, project), filter by minEpisodic,
// order by count DESC then user/project ASC for deterministic test
// expectations, cap at limit.
func (f *fakeDistillerStore) ListActiveDistillTenants(_ context.Context, minEpisodic, limit int) ([]memory.TenantRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	counts := make(map[[2]string]int)
	for _, m := range f.mems {
		if m.Type != memory.MemoryTypeEpisodic || m.DistilledAt != nil {
			continue
		}
		counts[[2]string{m.UserID, m.ProjectID}]++
	}
	refs := make([]memory.TenantRef, 0, len(counts))
	for k, c := range counts {
		if c < minEpisodic {
			continue
		}
		refs = append(refs, memory.TenantRef{UserID: k[0], ProjectID: k[1], Count: c})
	}
	// Stable order: count DESC, then user_id ASC, then project_id ASC.
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Count != refs[j].Count {
			return refs[i].Count > refs[j].Count
		}
		if refs[i].UserID != refs[j].UserID {
			return refs[i].UserID < refs[j].UserID
		}
		return refs[i].ProjectID < refs[j].ProjectID
	})
	if limit > 0 && len(refs) > limit {
		refs = refs[:limit]
	}
	return refs, nil
}

type MockLLM struct{}

func (m *MockLLM) GenerateContent(ctx context.Context, prompt string) (string, error) {
	return "Distilled: Users prefer fast tests", nil
}

func TestDistiller_Run(t *testing.T) {
	store := &fakeDistillerStore{
		mems: []memory.Memory{
			{ID: "e1", Type: memory.MemoryTypeEpisodic, Content: "Failed to run test due to timeout", Score: 1.0, UserID: "user1", ProjectID: "proj1"},
			{ID: "e2", Type: memory.MemoryTypeEpisodic, Content: "Fixed timeout by reducing wait time", Score: 1.0, UserID: "user1", ProjectID: "proj1"},
			{ID: "e3", Type: memory.MemoryTypeEpisodic, Content: "Replaced sleep with channel signaling for determinism", Score: 1.0, UserID: "user1", ProjectID: "proj1"},
		},
	}

	distiller := memory.NewDistiller(store, &MockLLM{}, nil, zap.NewNop(), memory.DistillerOptions{
		MinEpisodicToTrigger: 2,
	})

	n, err := distiller.Distill(context.Background(), "user1", "proj1")
	assert.NoError(t, err)
	assert.Equal(t, 3, n, "should report number of episodic memories distilled")

	var found bool
	for _, m := range store.mems {
		if m.Type == memory.MemoryTypeSemantic {
			found = true
			break
		}
	}
	assert.True(t, found, "Expected to find at least 1 semantic memory")
}

func TestDistiller_SkipsBelowThreshold(t *testing.T) {
	store := &fakeDistillerStore{
		mems: []memory.Memory{
			{ID: "e1", Type: memory.MemoryTypeEpisodic, Content: "single episode", Score: 1.0, UserID: "u", ProjectID: "p"},
		},
	}
	distiller := memory.NewDistiller(store, &MockLLM{}, nil, zap.NewNop(), memory.DistillerOptions{
		MinEpisodicToTrigger: 3,
	})

	n, err := distiller.Distill(context.Background(), "u", "p")
	assert.NoError(t, err)
	assert.Equal(t, 1, n)

	for _, m := range store.mems {
		assert.NotEqual(t, memory.MemoryTypeSemantic, m.Type, "should not have produced semantic memory")
	}
}

// TestDistiller_MarksConsumedEpisodes verifies the critical contract: a
// successful distill leaves the source episodes with DistilledAt set, so
// the *next* tick won't re-consume them. This is the explicit fix for
// P0 #1 (Distiller空转): before this, no code path ever wrote
// MemoryTypeEpisodic, and even after that fix, without MarkDistilled
// we'd produce N duplicate semantic rules over N ticks.
func TestDistiller_MarksConsumedEpisodes(t *testing.T) {
	store := &fakeDistillerStore{
		mems: []memory.Memory{
			{ID: "e1", Type: memory.MemoryTypeEpisodic, Content: "ep1", Score: 1.0, UserID: "u", ProjectID: "p"},
			{ID: "e2", Type: memory.MemoryTypeEpisodic, Content: "ep2", Score: 1.0, UserID: "u", ProjectID: "p"},
			{ID: "e3", Type: memory.MemoryTypeEpisodic, Content: "ep3", Score: 1.0, UserID: "u", ProjectID: "p"},
		},
	}
	distiller := memory.NewDistiller(store, &MockLLM{}, nil, zap.NewNop(), memory.DistillerOptions{
		MinEpisodicToTrigger: 2,
	})

	_, err := distiller.Distill(context.Background(), "u", "p")
	assert.NoError(t, err)

	// All 3 source episodes should now carry DistilledAt timestamps.
	for _, id := range []string{"e1", "e2", "e3"} {
		var found bool
		for _, m := range store.mems {
			if m.ID == id {
				found = true
				assert.NotNilf(t, m.DistilledAt, "episode %s should be marked as distilled", id)
			}
		}
		assert.True(t, found, "episode %s should still exist after distill", id)
	}
}

// TestDistiller_SkipsAlreadyDistilled is the integration check on the
// mark-then-skip contract: running Distill twice in a row must NOT
// produce a second semantic memory from the same source episodes.
// Without MarkDistilled this test fails with "produced 2 semantics" —
// exactly the bug P0 #1 exposes when the ticker fires hourly.
func TestDistiller_SkipsAlreadyDistilled(t *testing.T) {
	store := &fakeDistillerStore{
		mems: []memory.Memory{
			{ID: "e1", Type: memory.MemoryTypeEpisodic, Content: "ep1", Score: 1.0, UserID: "u", ProjectID: "p"},
			{ID: "e2", Type: memory.MemoryTypeEpisodic, Content: "ep2", Score: 1.0, UserID: "u", ProjectID: "p"},
		},
	}
	distiller := memory.NewDistiller(store, &MockLLM{}, nil, zap.NewNop(), memory.DistillerOptions{
		MinEpisodicToTrigger: 2,
	})

	// First tick: consume both episodes, produce 1 semantic.
	n1, err := distiller.Distill(context.Background(), "u", "p")
	assert.NoError(t, err)
	assert.Equal(t, 2, n1)

	// Second tick: nothing should be eligible (all episodes marked).
	// Distiller treats "0 < MinEpisodicToTrigger" as skip, returns 0.
	n2, err := distiller.Distill(context.Background(), "u", "p")
	assert.NoError(t, err)
	assert.Equal(t, 0, n2, "second tick should find no fresh episodes")

	// Total semantic memories should remain 1, not 2.
	var semCount int
	for _, m := range store.mems {
		if m.Type == memory.MemoryTypeSemantic {
			semCount++
		}
	}
	assert.Equal(t, 1, semCount, "no duplicate semantic memory across ticks")
}
