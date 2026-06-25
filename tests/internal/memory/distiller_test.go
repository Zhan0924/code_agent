package memory_test

import (
	"context"
	"testing"


	"github.com/agent/code_agent/internal/memory"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

type MockLLM struct{}
func (m *MockLLM) GenerateContent(ctx context.Context, prompt string) (string, error) {
	return "Distilled: Users prefer fast tests", nil
}

func TestDistiller_Run(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.FlushDB(context.Background())
	
	hot := memory.NewRedisHot(rdb, zap.NewNop())
	bb := memory.NewBlackboard(rdb, zap.NewNop())
	
	distiller := memory.NewDistiller(hot, &MockLLM{}, bb)
	
	m1 := &memory.Memory{ID: "e1", Type: memory.MemoryTypeEpisodic, Content: "Failed to run test due to timeout", Score: 1.0, UserID: "user1", ProjectID: "proj1"}
	m2 := &memory.Memory{ID: "e2", Type: memory.MemoryTypeEpisodic, Content: "Fixed timeout by reducing wait time", Score: 1.0, UserID: "user1", ProjectID: "proj1"}
	hot.Store(context.Background(), m1)
	hot.Store(context.Background(), m2)
	
	err := distiller.Distill(context.Background(), "user1", "proj1")
	assert.NoError(t, err)
	
	mems, _ := hot.Retrieve(context.Background(), "user1", "proj1", 10)
	var found bool
	for _, m := range mems {
		if m.Type == memory.MemoryTypeSemantic {
			found = true
			break
		}
	}
	assert.True(t, found, "Expected to find at least 1 semantic memory")
}
