package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/session"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type recordingMemoryRetriever struct {
	boostCalls []boostCall
}

type boostCall struct {
	refs  []memory.TouchRef
	boost float64
}

func (r *recordingMemoryRetriever) Retrieve(_ context.Context, _, _, _ string, _ int) ([]MemoryEntry, error) {
	return nil, nil
}

func (r *recordingMemoryRetriever) RetrieveByType(_ context.Context, _, _, _, _ string, _ int) ([]MemoryEntry, error) {
	return nil, nil
}

func (r *recordingMemoryRetriever) BoostScoreBatch(_ context.Context, refs []memory.TouchRef, boost float64) error {
	cp := make([]memory.TouchRef, len(refs))
	copy(cp, refs)
	r.boostCalls = append(r.boostCalls, boostCall{refs: cp, boost: boost})
	return nil
}

type hybridMemoryAdapter struct {
	store *memory.HybridStore
}

func (a *hybridMemoryAdapter) Retrieve(ctx context.Context, userID, projectID, query string, limit int) ([]MemoryEntry, error) {
	mems, err := a.store.Retrieve(ctx, userID, projectID, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]MemoryEntry, 0, len(mems))
	for _, m := range mems {
		out = append(out, MemoryEntry{ID: m.ID, Type: string(m.Type), Content: m.Content, Score: m.Score})
	}
	return out, nil
}

func (a *hybridMemoryAdapter) RetrieveByType(ctx context.Context, userID, projectID, memType, query string, limit int) ([]MemoryEntry, error) {
	mems, err := a.store.RetrieveByType(ctx, userID, projectID, memType, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]MemoryEntry, 0, len(mems))
	for _, m := range mems {
		out = append(out, MemoryEntry{ID: m.ID, Type: string(m.Type), Content: m.Content, Score: m.Score})
	}
	return out, nil
}

func (a *hybridMemoryAdapter) BoostScoreBatch(ctx context.Context, refs []memory.TouchRef, boost float64) error {
	return a.store.BoostScoreBatch(ctx, refs, boost)
}

func newCitationBoostHarness(t *testing.T) (*Orchestrator, *session.Manager, *recordingMemoryRetriever) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	sessCfg := &config.SessionConfig{TTL: time.Hour}
	sessMgr := session.NewManager(rdb, sessCfg, zap.NewNop())
	rec := &recordingMemoryRetriever{}

	o := &Orchestrator{
		sessionMgr:  sessMgr,
		memoryStore: rec,
		logger:      zap.NewNop(),
	}
	return o, sessMgr, rec
}

func TestBoostCitedMemories_BoostsUniqueIDs(t *testing.T) {
	o, sessMgr, rec := newCitationBoostHarness(t)
	ctx := context.Background()

	sess, err := sessMgr.Create(ctx, "alice", "p1")
	require.NoError(t, err)

	o.boostCitedMemories(ctx, sess.ID, "Answer uses [mem:abc-123] and again [mem:abc-123] plus [mem:xyz]")

	require.Len(t, rec.boostCalls, 1)
	assert.InDelta(t, 0.05, rec.boostCalls[0].boost, 1e-9)
	require.Len(t, rec.boostCalls[0].refs, 2)

	ids := map[string]bool{}
	for _, ref := range rec.boostCalls[0].refs {
		assert.Equal(t, "alice", ref.UserID)
		assert.Equal(t, "p1", ref.ProjectID)
		ids[ref.ID] = true
	}
	assert.True(t, ids["abc-123"])
	assert.True(t, ids["xyz"])
}

func TestBoostCitedMemories_NoCitationSkipsBoost(t *testing.T) {
	o, sessMgr, rec := newCitationBoostHarness(t)
	ctx := context.Background()

	sess, err := sessMgr.Create(ctx, "alice", "p1")
	require.NoError(t, err)

	o.boostCitedMemories(ctx, sess.ID, "plain answer without memory tags")
	assert.Empty(t, rec.boostCalls)
}

func TestBoostCitedMemories_NilStoreIsNoOp(t *testing.T) {
	o, sessMgr, _ := newCitationBoostHarness(t)
	o.memoryStore = nil
	ctx := context.Background()

	sess, err := sessMgr.Create(ctx, "alice", "p1")
	require.NoError(t, err)

	o.boostCitedMemories(ctx, sess.ID, "uses [mem:abc]")
}

func TestBoostCitedMemories_UpdatesHybridStoreScore(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	hot := memory.NewRedisHot(rdb, zap.NewNop())
	store := memory.NewHybridStore(hot, nil, zap.NewNop())

	sessMgr := session.NewManager(rdb, &config.SessionConfig{TTL: time.Hour}, zap.NewNop())
	o := &Orchestrator{
		sessionMgr: sessMgr,
		memoryStore: &hybridMemoryAdapter{store: store},
		logger:      zap.NewNop(),
	}

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, hot.Store(ctx, &memory.Memory{
		ID: "m1", UserID: "alice", ProjectID: "p1",
		Type: memory.MemoryPreference, Content: "pref tabs", Score: 0.5,
		LastAccessedAt: now,
	}))

	sess, err := sessMgr.Create(ctx, "alice", "p1")
	require.NoError(t, err)

	o.boostCitedMemories(ctx, sess.ID, "Based on [mem:m1] you prefer tabs.")

	got, err := hot.Retrieve(ctx, "alice", "p1", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InDelta(t, 0.55, got[0].Score, 1e-9)
}
