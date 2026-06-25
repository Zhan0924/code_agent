package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestRedisHot_Decay(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.FlushDB(context.Background())
	
	hot := memory.NewRedisHot(rdb, zap.NewNop())
	
	m := &memory.Memory{
		ID:        "mem-123",
		UserID:    "user1",
		ProjectID: "proj1",
		Score:     1.0,
		UpdatedAt: time.Now().Add(-40 * 24 * time.Hour), // 40 days old
	}
	_ = hot.Store(context.Background(), m)
	
	count, err := hot.Decay(30*24*time.Hour, 0.8)
	assert.NoError(t, err)
	assert.Equal(t, 1, count)
	
	retrieved, _ := hot.Retrieve(context.Background(), "user1", "proj1", 10)
	if len(retrieved) > 0 {
		assert.InDelta(t, 0.8, retrieved[0].Score, 0.01)
	} else {
		t.Fatal("Expected to retrieve 1 memory, got 0")
	}
}
