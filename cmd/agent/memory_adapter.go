package main

import (
	"context"

	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/orchestrator"
	"github.com/agent/code_agent/internal/store"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// memoryAdapter bridges memory.HybridStore to orchestrator.MemoryRetriever.
type memoryAdapter struct {
	store *memory.HybridStore
}

func (a *memoryAdapter) Retrieve(ctx context.Context, userID, projectID, query string, limit int) ([]orchestrator.MemoryEntry, error) {
	memories, err := a.store.Retrieve(ctx, userID, projectID, query, limit)
	if err != nil {
		return nil, err
	}

	entries := make([]orchestrator.MemoryEntry, 0, len(memories))
	for _, m := range memories {
		entries = append(entries, orchestrator.MemoryEntry{
			Type:    string(m.Type),
			Content: m.Content,
			Score:   m.Score,
		})
	}
	return entries, nil
}

// NewMemoryAdapter creates a memory adapter backed by Redis hot tier.
// PG cold tier can be added later once store.Store exposes the underlying sql.DB.
func NewMemoryAdapter(rdb *redis.Client, _ *store.Store, logger *zap.Logger) orchestrator.MemoryRetriever {
	if rdb == nil {
		return nil
	}

	hot := memory.NewRedisHot(rdb, logger)
	hybrid := memory.NewHybridStore(hot, nil, logger)
	return &memoryAdapter{store: hybrid}
}
