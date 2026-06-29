package memory

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRedisHot_BoostScoreBatch_Clamped(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	hot := NewRedisHot(rdb, zap.NewNop())

	ctx := context.Background()
	now := time.Now().UTC()

	seed := []Memory{
		{ID: "high", UserID: "u1", ProjectID: "p1", Type: MemoryPreference, Content: "high", Score: 0.95, LastAccessedAt: now},
		{ID: "low", UserID: "u1", ProjectID: "p1", Type: MemoryPreference, Content: "low", Score: 0.05, LastAccessedAt: now},
	}
	for i := range seed {
		require.NoError(t, hot.Store(ctx, &seed[i]))
	}

	require.NoError(t, hot.BoostScoreBatch(ctx, []TouchRef{
		{UserID: "u1", ProjectID: "p1", ID: "high"},
	}, 0.2))
	require.NoError(t, hot.BoostScoreBatch(ctx, []TouchRef{
		{UserID: "u1", ProjectID: "p1", ID: "low"},
	}, -0.2))

	got, err := hot.Retrieve(ctx, "u1", "p1", 10)
	require.NoError(t, err)
	byID := map[string]Memory{}
	for _, m := range got {
		byID[m.ID] = m
	}
	assert.InDelta(t, MaxMemoryScore, byID["high"].Score, 1e-9)
	assert.InDelta(t, MinMemoryScore, byID["low"].Score, 1e-9)
}
