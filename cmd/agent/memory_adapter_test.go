package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeDiscoverStore stubs the memory.DistillerStore surface that
// buildDistillTenants touches; we don't need ListByType / Store /
// MarkDistilled here, only the discovery call.
type fakeDiscoverStore struct {
	tenants []memory.TenantRef
	err     error
	gotMin  int
	gotLim  int
}

func (f *fakeDiscoverStore) ListByType(_ context.Context, _, _ string, _ memory.MemoryType, _ int) ([]memory.Memory, error) {
	return nil, nil
}
func (f *fakeDiscoverStore) Store(_ context.Context, _ *memory.Memory) error { return nil }
func (f *fakeDiscoverStore) MarkDistilled(_ context.Context, _ []string) error {
	return nil
}
func (f *fakeDiscoverStore) DeleteOldEpisodic(_ context.Context, _ time.Duration) (int64, error) {
	return 0, nil
}
func (f *fakeDiscoverStore) ListActiveDistillTenants(_ context.Context, minEpisodic, limit int) ([]memory.TenantRef, error) {
	f.gotMin = minEpisodic
	f.gotLim = limit
	if f.err != nil {
		return nil, f.err
	}
	if limit > 0 && len(f.tenants) > limit {
		return f.tenants[:limit], nil
	}
	return f.tenants, nil
}

// TestBuildDistillTenants_AutoDiscoverOnly: classic multi-tenant case.
// No Targets, AutoDiscover on → all tenants come from PG.
func TestBuildDistillTenants_AutoDiscoverOnly(t *testing.T) {
	store := &fakeDiscoverStore{tenants: []memory.TenantRef{
		{UserID: "a", ProjectID: "p1", Count: 5},
		{UserID: "b", ProjectID: "p1", Count: 3},
	}}
	got := buildDistillTenants(context.Background(), store, config.MemoryDistillConfig{
		Enabled:      true,
		AutoDiscover: true,
	}, 10, 2, zap.NewNop())
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].UserID)
	assert.Equal(t, "b", got[1].UserID)
	assert.Equal(t, 2, store.gotMin)
	assert.Equal(t, 10, store.gotLim)
}

// TestBuildDistillTenants_ForcedInclusion: static Targets always appear
// in the output, even if PG returns higher-count tenants that would
// otherwise dominate the cap.
func TestBuildDistillTenants_ForcedInclusion(t *testing.T) {
	store := &fakeDiscoverStore{tenants: []memory.TenantRef{
		{UserID: "discovered", ProjectID: "p1", Count: 100},
	}}
	got := buildDistillTenants(context.Background(), store, config.MemoryDistillConfig{
		Enabled:      true,
		AutoDiscover: true,
		Targets: []config.MemoryDistillTarget{
			{UserID: "forced", ProjectID: "p1"},
		},
	}, 2, 2, zap.NewNop())
	require.Len(t, got, 2)
	assert.Equal(t, "forced", got[0].UserID, "static targets prepend the list")
	assert.Equal(t, "discovered", got[1].UserID)
}

// TestBuildDistillTenants_Dedup: a target that PG also discovers shouldn't
// appear twice. The static-first ordering means Targets win the spot.
func TestBuildDistillTenants_Dedup(t *testing.T) {
	store := &fakeDiscoverStore{tenants: []memory.TenantRef{
		{UserID: "alice", ProjectID: "p1", Count: 7},
		{UserID: "bob", ProjectID: "p1", Count: 5},
	}}
	got := buildDistillTenants(context.Background(), store, config.MemoryDistillConfig{
		Enabled:      true,
		AutoDiscover: true,
		Targets: []config.MemoryDistillTarget{
			{UserID: "alice", ProjectID: "p1"},
		},
	}, 10, 2, zap.NewNop())
	require.Len(t, got, 2, "alice de-duped, bob added once")
	assert.Equal(t, "alice", got[0].UserID)
	assert.Equal(t, "bob", got[1].UserID)
}

// TestBuildDistillTenants_Cap: total output must not exceed
// MaxTenantsPerTick even when Targets + discovered together overflow.
func TestBuildDistillTenants_Cap(t *testing.T) {
	store := &fakeDiscoverStore{tenants: []memory.TenantRef{
		{UserID: "d1", ProjectID: "p1", Count: 9},
		{UserID: "d2", ProjectID: "p1", Count: 8},
		{UserID: "d3", ProjectID: "p1", Count: 7},
	}}
	got := buildDistillTenants(context.Background(), store, config.MemoryDistillConfig{
		Enabled:      true,
		AutoDiscover: true,
		Targets: []config.MemoryDistillTarget{
			{UserID: "t1", ProjectID: "p1"},
			{UserID: "t2", ProjectID: "p1"},
		},
	}, 3, 2, zap.NewNop())
	require.Len(t, got, 3, "cap=3 must hold (2 forced + 1 discovered)")
	assert.Equal(t, "t1", got[0].UserID)
	assert.Equal(t, "t2", got[1].UserID)
	assert.Equal(t, "d1", got[2].UserID, "highest-count discovered tenant fills the last slot")
}

// TestBuildDistillTenants_AutoDiscoverOff: with AutoDiscover=false the
// loop must never call ListActiveDistillTenants — keeps the legacy
// "static targets only" semantics exactly the way operators relying on
// it expect.
func TestBuildDistillTenants_AutoDiscoverOff(t *testing.T) {
	store := &fakeDiscoverStore{tenants: []memory.TenantRef{
		{UserID: "nope", ProjectID: "p1", Count: 100},
	}}
	got := buildDistillTenants(context.Background(), store, config.MemoryDistillConfig{
		Enabled:      true,
		AutoDiscover: false,
		Targets: []config.MemoryDistillTarget{
			{UserID: "only", ProjectID: "p1"},
		},
	}, 10, 2, zap.NewNop())
	require.Len(t, got, 1)
	assert.Equal(t, "only", got[0].UserID)
	assert.Equal(t, 0, store.gotLim, "ListActiveDistillTenants must not be called when AutoDiscover=false")
}

// TestBuildDistillTenants_DiscoverError: PG hiccups must NOT take down
// the tick — we still distill the static Targets and log+continue.
func TestBuildDistillTenants_DiscoverError(t *testing.T) {
	store := &fakeDiscoverStore{err: errors.New("pg down")}
	got := buildDistillTenants(context.Background(), store, config.MemoryDistillConfig{
		Enabled:      true,
		AutoDiscover: true,
		Targets: []config.MemoryDistillTarget{
			{UserID: "stillruns", ProjectID: "p1"},
		},
	}, 10, 2, zap.NewNop())
	require.Len(t, got, 1)
	assert.Equal(t, "stillruns", got[0].UserID)
}

// TestBuildDistillTenants_EmptyTargetEntriesIgnored: yaml typos
// (empty user/project) must not silently distill "" — would otherwise
// produce orphan semantic memories for user="".
func TestBuildDistillTenants_EmptyTargetEntriesIgnored(t *testing.T) {
	store := &fakeDiscoverStore{}
	got := buildDistillTenants(context.Background(), store, config.MemoryDistillConfig{
		Enabled:      true,
		AutoDiscover: false,
		Targets: []config.MemoryDistillTarget{
			{UserID: "", ProjectID: "p1"},
			{UserID: "u", ProjectID: ""},
			{UserID: "u", ProjectID: "p1"},
		},
	}, 10, 2, zap.NewNop())
	require.Len(t, got, 1)
	assert.Equal(t, "u", got[0].UserID)
	assert.Equal(t, "p1", got[0].ProjectID)
}
