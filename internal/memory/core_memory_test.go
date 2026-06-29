package memory

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newTestRedisCoreMemory(t *testing.T) (*RedisCoreMemory, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	return NewRedisCoreMemory(rdb, zap.NewNop()), mr
}

func TestRedisCoreMemory_PIIMaskOnAppend(t *testing.T) {
	cm, mr := newTestRedisCoreMemory(t)
	ctx := context.Background()
	const (
		userID    = "u_pii"
		projectID = "p_pii"
		secret    = "AKIAIOSFODNN7EXAMPLE"
	)

	err := cm.AppendToSectionScoped(ctx, userID, projectID, CoreScopeProject, "human_context", "phone backup key "+secret)
	require.NoError(t, err)

	raw, err := mr.Get("core_memory:project:u_pii:p_pii")
	require.NoError(t, err)

	var mem CoreMemory
	require.NoError(t, json.Unmarshal([]byte(raw), &mem))
	got := mem.Sections["human_context"].Content
	assert.NotContains(t, got, secret, "AWS key must not persist in core memory")
	assert.Contains(t, got, "[REDACTED:AWS_KEY]")
}

func TestRedisCoreMemory_PIIMaskOnReplace(t *testing.T) {
	cm, mr := newTestRedisCoreMemory(t)
	ctx := context.Background()
	const (
		userID    = "u_pii2"
		projectID = "p_pii2"
		secret    = "sk-abcdefghijklmnopqrstuvwxyz123456"
	)

	require.NoError(t, cm.AppendToSectionScoped(ctx, userID, projectID, CoreScopeProject, "persona", "placeholder token"))
	require.NoError(t, cm.ReplaceInSectionScoped(ctx, userID, projectID, CoreScopeProject, "persona", "placeholder token", secret))

	raw, err := mr.Get("core_memory:project:u_pii2:p_pii2")
	require.NoError(t, err)

	var mem CoreMemory
	require.NoError(t, json.Unmarshal([]byte(raw), &mem))
	got := mem.Sections["persona"].Content
	assert.NotContains(t, got, secret)
	assert.Contains(t, got, "[REDACTED:API_KEY]")
}
