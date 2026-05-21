// Package rag - reconnect.go
// [OPT-14] Periodic Qdrant reconnection mechanism.
// If Qdrant is unavailable at startup, the RAG engine is disabled.
// This module periodically retries to establish the connection so
// that RAG becomes available without requiring a full service restart.
package rag

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap"
)

// ReconnectManager periodically attempts to reconnect to Qdrant
// if it was unavailable at startup or lost connection later.
type ReconnectManager struct {
	qdrantCfg *config.QdrantConfig
	ragCfg    *config.RAGConfig
	engine    atomic.Pointer[Engine]
	embedder  Embedder
	logger    *zap.Logger
	stopCh    chan struct{}
}

// NewReconnectManager creates a manager that watches Qdrant connectivity.
func NewReconnectManager(
	qdrantCfg *config.QdrantConfig,
	ragCfg *config.RAGConfig,
	embedder Embedder,
	logger *zap.Logger,
) *ReconnectManager {
	return &ReconnectManager{
		qdrantCfg: qdrantCfg,
		ragCfg:    ragCfg,
		embedder:  embedder,
		logger:    logger.With(zap.String("component", "rag-reconnect")),
		stopCh:    make(chan struct{}),
	}
}

// SetEngine atomically sets the current RAG engine (may be nil).
func (rm *ReconnectManager) SetEngine(e *Engine) {
	rm.engine.Store(e)
}

// GetEngine atomically returns the current RAG engine (may be nil).
func (rm *ReconnectManager) GetEngine() *Engine {
	return rm.engine.Load()
}

// Start begins the periodic reconnection loop.
// It checks every interval whether Qdrant is reachable and, if a new
// connection is established, creates a new Engine and stores it atomically.
func (rm *ReconnectManager) Start(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if rm.GetEngine() != nil {
					// Already connected — verify health
					continue
				}
				rm.tryReconnect(ctx)
			case <-rm.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop halts the reconnection loop.
func (rm *ReconnectManager) Stop() {
	close(rm.stopCh)
}

func (rm *ReconnectManager) tryReconnect(ctx context.Context) {
	rm.logger.Info("attempting Qdrant reconnection",
		zap.String("addr", rm.qdrantCfg.Addr))

	store, err := NewQdrantStore(rm.qdrantCfg, rm.logger)
	if err != nil {
		rm.logger.Warn("Qdrant reconnection failed", zap.Error(err))
		return
	}

	engine := NewEngine(rm.embedder, store, nil, rm.ragCfg, rm.logger)
	rm.SetEngine(engine)

	rm.logger.Info("Qdrant reconnected successfully — RAG engine now available",
		zap.String("collection", rm.qdrantCfg.Collection))
}
