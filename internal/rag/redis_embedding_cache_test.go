package rag

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRedisEmbeddingCache(t *testing.T) {
	// Start miniredis
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	// Create Redis client
	rdb := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})
	defer rdb.Close()

	logger := zap.NewNop()
	cache := NewRedisEmbeddingCache(rdb, "test-model", logger)

	ctx := context.Background()

	// Test miss
	vec, ok := cache.Get(ctx, "key1")
	assert.False(t, ok)
	assert.Nil(t, vec)

	// Test put and get
	testVec := []float32{1.0, 2.0, 3.0, 4.0}
	cache.Put(ctx, "key1", testVec)

	vec, ok = cache.Get(ctx, "key1")
	assert.True(t, ok)
	assert.Equal(t, testVec, vec)

	// Test stats
	stats := cache.Stats()
	assert.Equal(t, uint64(1), stats.Hits)
	assert.Equal(t, uint64(1), stats.Misses)
}

func TestTieredCache(t *testing.T) {
	// Start miniredis
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	// Create Redis client
	rdb := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})
	defer rdb.Close()

	logger := zap.NewNop()
	l1 := NewMemoryEmbeddingCache(100, "test-model", logger)
	l2 := NewRedisEmbeddingCache(rdb, "test-model", logger)
	cache := NewTieredCache(l1, l2, logger)

	ctx := context.Background()

	// Test L1 miss, L2 miss
	vec, ok := cache.Get(ctx, "key1")
	assert.False(t, ok)
	assert.Nil(t, vec)

	// Put into tiered cache (writes to both L1 and L2)
	testVec := []float32{1.0, 2.0, 3.0, 4.0}
	cache.Put(ctx, "key1", testVec)

	// Get should hit L1
	vec, ok = cache.Get(ctx, "key1")
	assert.True(t, ok)
	assert.Equal(t, testVec, vec)

	// Clear L1 to test L2 backfill
	l1 = NewMemoryEmbeddingCache(100, "test-model", logger)
	cache = NewTieredCache(l1, l2, logger)

	// Get should miss L1, hit L2, and backfill L1
	vec, ok = cache.Get(ctx, "key1")
	assert.True(t, ok)
	assert.Equal(t, testVec, vec)

	// Next get should hit L1
	vec, ok = cache.Get(ctx, "key1")
	assert.True(t, ok)
	assert.Equal(t, testVec, vec)
}

func TestSerializeDeserialize(t *testing.T) {
	testVec := []float32{1.5, -2.3, 0.0, 999.999}
	data := serializeFloat32(testVec)
	vec, err := deserializeFloat32(data)
	require.NoError(t, err)
	assert.Equal(t, testVec, vec)
}
