package memory

import (
	"context"

	"go.uber.org/zap"
)

// defaultDedupOversample is the candidate budget HybridStore.Store gives the
// vector search when looking for conflicts (REAUDIT-P2-1 configurable).
const defaultDedupOversample = 10

// Embedder generates vector embeddings from text.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// HybridStore combines Redis (hot) and PostgreSQL (cold) memory stores.
// Reads fuse hot + cold via RRF; writes go to both with best-effort
// consistency + outbox-style event publishing.
//
// REAUDIT-P1-1: methods are split across hybrid_*.go files by domain
// (embed, store, retrieve, list, admin, queues, decay, dedup, rrf).
type HybridStore struct {
	hot        *RedisHot
	cold       *PGCold
	embedder   Embedder
	logger     *zap.Logger
	blackboard *Blackboard
	resolver   *ConflictResolver

	// touchQueue + accessOpts implement the P0 #4 read-path
	// access_count / last_accessed_at batcher. nil queue means
	// "feature disabled" — enqueueTouches becomes a no-op.
	//
	// P0 #5: the queue carries full TouchRef (not just ID) so the
	// batcher can refresh hot's JSON in place — hot's Redis key is
	// `memory:<userID>:<projectID>:<id>` and the prefix is required
	// to reconstruct it.
	touchQueue chan TouchRef
	accessOpts AccessBatcherOptions

	// promoteQueue + promoteOpts implement the P1 #8 read-path
	// cold→hot back-fill. nil queue means "feature disabled" —
	// enqueuePromote becomes a no-op. Channel carries full Memory
	// values (not just IDs) because RedisHot.PromoteBatch needs the
	// JSON to SET.
	promoteQueue chan Memory
	promoteOpts  PromoteOptions

	// demoteThreshold is the P1 #8 hot-eviction floor used by Decay.
	// Zero (the default) disables demotion — Decay keeps the
	// pre-fix behavior (SET with reduced score, KeepTTL). Set via
	// SetDemoteThreshold from main.go.
	demoteThreshold float64

	dedupOversample int

	masker *PIIMasker
}

// NewHybridStore creates a hybrid memory store.
func NewHybridStore(hot *RedisHot, cold *PGCold, logger *zap.Logger) *HybridStore {
	return &HybridStore{
		hot:             hot,
		cold:            cold,
		logger:          logger.With(zap.String("component", "memory.hybrid")),
		resolver:        NewConflictResolver(cold),
		masker:          NewPIIMasker(),
		dedupOversample: defaultDedupOversample,
	}
}

// SetDedupOversample overrides conflict-detection vector candidate oversample K.
func (h *HybridStore) SetDedupOversample(n int) {
	if n > 0 {
		h.dedupOversample = n
	}
}

// DedupOversample returns the effective oversample K (default 10).
func (h *HybridStore) DedupOversample() int {
	if h.dedupOversample <= 0 {
		return defaultDedupOversample
	}
	return h.dedupOversample
}

// SetBlackboard sets the blackboard for publishing events.
func (h *HybridStore) SetBlackboard(b *Blackboard) {
	h.blackboard = b
}

// SetEmbedder injects an embedder for semantic operations.
func (h *HybridStore) SetEmbedder(e Embedder) {
	h.embedder = e
}

// SetConflictResolver allows callers (main.go) to swap in a custom
// resolver — e.g. one with project-specific thresholds.
func (h *HybridStore) SetConflictResolver(r *ConflictResolver) {
	if r != nil {
		h.resolver = r
	}
}
