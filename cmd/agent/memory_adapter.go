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

// NewMemoryAdapter creates a memory adapter backed by Redis hot tier and PG cold tier.
func NewMemoryAdapter(rdb *redis.Client, pgStore *store.Store, embedder memory.Embedder, logger *zap.Logger) orchestrator.MemoryRetriever {
	if rdb == nil {
		return nil
	}

	hot := memory.NewRedisHot(rdb, logger)

	var cold *memory.PGCold
	if pgStore != nil {
		cold = memory.NewPGCold(pgStore.DB(), logger)
		if err := cold.Migrate(); err != nil {
			logger.Warn("memory cold store migration failed", zap.Error(err))
			cold = nil
		}
	}

	hybrid := memory.NewHybridStore(hot, cold, logger)
	if embedder != nil {
		hybrid.SetEmbedder(embedder)
	}
	return &memoryAdapter{store: hybrid}
}
