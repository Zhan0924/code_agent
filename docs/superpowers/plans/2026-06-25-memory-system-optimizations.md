# Memory System Optimizations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Evolve the current memory system to support state-of-the-art multi-tier persistence, temporal decay, and semantic distillation, aligning with 2026 research on scalable, persistent agent architectures.

**Architecture:** We will introduce a Semantic Distillation background worker to convert episodic logs into high-level rules (inspired by MemGovern), add a Memory Decay cron mechanism to prevent stale context hallucination (inspired by Mem0), and establish explicit graph relations between memory entities to complement our existing RAG hybrid store (inspired by A-Mem).

**Tech Stack:** Go, Redis, PostgreSQL, OpenAI (for distillation)

---

### Task 1: Memory Decay Mechanism

**Files:**
- Modify: `internal/memory/types.go:1-50`
- Modify: `internal/memory/redis_hot.go:80-120`
- Test: `tests/internal/memory/decay_test.go`

- [ ] **Step 1: Write the failing test for Redis decay**

```go
package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

func TestRedisHot_Decay(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.FlushDB(context.Background())
	
	hot := memory.NewRedisHot(rdb, nil)
	
	m := &memory.Memory{
		ID:        "mem-123",
		UserID:    "user1",
		ProjectID: "proj1",
		Score:     1.0,
		UpdatedAt: time.Now().Add(-40 * 24 * time.Hour), // 40 days old
	}
	_ = hot.Store(m)
	
	count, err := hot.Decay(30*24*time.Hour, 0.8)
	assert.NoError(t, err)
	assert.Equal(t, 1, count)
	
	retrieved, _ := hot.Retrieve(context.Background(), "user1", "proj1", "", 10)
	assert.InDelta(t, 0.8, retrieved[0].Score, 0.01)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./tests/internal/memory -run TestRedisHot_Decay`
Expected: FAIL with "Decay not implemented" or similar error depending on current stub.

- [ ] **Step 3: Write minimal implementation in redis_hot.go**

```go
// Add to internal/memory/redis_hot.go
func (r *RedisHot) Decay(olderThan time.Duration, factor float64) (int, error) {
	ctx := context.Background()
	keys, err := r.client.Keys(ctx, "mem:*").Result()
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().Add(-olderThan)
	count := 0

	for _, key := range keys {
		data, err := r.client.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}
		var m Memory
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}

		if m.UpdatedAt.Before(cutoff) {
			m.Score *= float32(factor)
			m.UpdatedAt = time.Now()
			updatedData, _ := json.Marshal(m)
			r.client.Set(ctx, key, updatedData, 0)
			count++
		}
	}
	return count, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./tests/internal/memory -run TestRedisHot_Decay`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/memory/redis_hot.go tests/internal/memory/decay_test.go
git commit -m "feat(memory): implement memory decay mechanism in RedisHot"
```

### Task 2: Episodic to Semantic Distillation Background Worker

**Files:**
- Create: `internal/memory/distiller.go`
- Test: `tests/internal/memory/distiller_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/stretchr/testify/assert"
)

type MockLLM struct{}
func (m *MockLLM) GenerateContent(ctx context.Context, prompt string) (string, error) {
	return "Distilled: Users prefer fast tests", nil
}

func TestDistiller_Run(t *testing.T) {
	hot := memory.NewRedisHot(redis.NewClient(&redis.Options{Addr: "localhost:6379"}), nil)
	bb := memory.NewBlackboard(redis.NewClient(&redis.Options{Addr: "localhost:6379"}), nil)
	
	distiller := memory.NewDistiller(hot, &MockLLM{}, bb)
	
	// Add episodic memories
	m1 := &memory.Memory{ID: "e1", Type: memory.MemoryTypeEpisodic, Content: "Failed to run test due to timeout"}
	m2 := &memory.Memory{ID: "e2", Type: memory.MemoryTypeEpisodic, Content: "Fixed timeout by reducing wait time"}
	hot.Store(m1)
	hot.Store(m2)
	
	err := distiller.Distill(context.Background(), "user1", "proj1")
	assert.NoError(t, err)
	
	// Check if semantic memory was created
	mems, _ := hot.Retrieve(context.Background(), "user1", "proj1", "Distilled", 10)
	assert.GreaterOrEqual(t, len(mems), 1)
	assert.Equal(t, memory.MemoryTypeSemantic, mems[0].Type)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./tests/internal/memory -run TestDistiller_Run`
Expected: FAIL with "undefined: NewDistiller"

- [ ] **Step 3: Write minimal implementation**

```go
// Create internal/memory/distiller.go
package memory

import (
	"context"
	"fmt"
	"time"
)

type LLMClient interface {
	GenerateContent(ctx context.Context, prompt string) (string, error)
}

type Distiller struct {
	store      *RedisHot
	llm        LLMClient
	blackboard *Blackboard
}

func NewDistiller(store *RedisHot, llm LLMClient, bb *Blackboard) *Distiller {
	return &Distiller{store: store, llm: llm, blackboard: bb}
}

func (d *Distiller) Distill(ctx context.Context, userID, projectID string) error {
	// Retrieve episodic memories
	mems, err := d.store.Retrieve(ctx, userID, projectID, "", 50)
	if err != nil {
		return err
	}
	
	var episodicContext string
	for _, m := range mems {
		if m.Type == MemoryTypeEpisodic {
			episodicContext += m.Content + "\n"
		}
	}
	
	if episodicContext == "" {
		return nil // Nothing to distill
	}
	
	prompt := fmt.Sprintf("Distill the following episodic memories into a single semantic rule:\n%s", episodicContext)
	distilled, err := d.llm.GenerateContent(ctx, prompt)
	if err != nil {
		return err
	}
	
	semanticMem := &Memory{
		ID:        fmt.Sprintf("sem-%d", time.Now().UnixNano()),
		UserID:    userID,
		ProjectID: projectID,
		Type:      MemoryTypeSemantic,
		Content:   distilled,
		Score:     1.0,
		UpdatedAt: time.Now(),
	}
	
	if err := d.store.Store(semanticMem); err != nil {
		return err
	}
	
	if d.blackboard != nil {
		_ = d.blackboard.Publish(ctx, "distilled", semanticMem)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./tests/internal/memory -run TestDistiller_Run`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/memory/distiller.go tests/internal/memory/distiller_test.go
git commit -m "feat(memory): implement episodic to semantic memory distiller"
```
