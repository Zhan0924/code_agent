package memory

import (
	"context"
	"fmt"

	"github.com/agent/code_agent/internal/metrics"
	"go.uber.org/zap"
)

// embedText generates an embedding for a single text, returning nil on failure.
func (h *HybridStore) embedText(ctx context.Context, text string) []float32 {
	if h.embedder == nil {
		metrics.MemoryFailuresTotal.WithLabelValues("embedder", "embed", "warn").Inc()
		h.logger.Warn("embedder is nil, retrieval will degrade to ILIKE",
			zap.String("audit_id", "REAUDIT-P0-4"),
			zap.String("op", "embedder_unavailable"),
			zap.String("result", "degraded"))
		return nil
	}
	vecs, err := h.embedder.Embed(ctx, []string{text})
	if err != nil || len(vecs) == 0 {
		metrics.MemoryFailuresTotal.WithLabelValues("embedder", "embed", "error").Inc()
		h.logger.Warn("embedding failed (retrieval quality degraded)",
			zap.String("audit_id", "REAUDIT-P0-4"),
			zap.String("op", "embed_failed"),
			zap.Error(err),
			zap.String("result", "degraded"))
		return nil
	}
	return vecs[0]
}

// recordRetrieveEmbedderDegraded emits REAUDIT-P0-4 observability when
// semantic retrieve falls back to ILIKE because embedText returned nil.
func (h *HybridStore) recordRetrieveEmbedderDegraded(ctx context.Context, userID, projectID, query string) {
	reason := "embedder_failed"
	if h.embedder == nil {
		reason = "embedder_nil"
	}
	metrics.MemoryRetrieveDegradedTotal.WithLabelValues(reason).Inc()
	h.logger.Warn("retrieve degraded to ILIKE text search",
		zap.String("audit_id", "REAUDIT-P0-4"),
		zap.String("op", "retrieve_degraded"),
		zap.String("reason", reason),
		zap.String("user_id", userID),
		zap.String("project_id", projectID),
		zap.String("query", query),
		zap.String("result", "degraded"))
}

// RetrieveWithEmbedder runs Retrieve using a temporary embedder override.
// Dev-only smoke tests use this to exercise embedder failure paths without
// mutating the production embedder wired at startup.
func (h *HybridStore) RetrieveWithEmbedder(ctx context.Context, emb Embedder, userID, projectID, query string, limit int) ([]Memory, error) {
	prev := h.embedder
	h.embedder = emb
	defer func() { h.embedder = prev }()
	return h.Retrieve(ctx, userID, projectID, query, limit)
}

// TestFailingEmbedder is a dev-only embedder that always errors. Used by
// verify-reaudit-p0-4.sh to exercise degrade observability without
// mutating the production embedder wired at startup.
type TestFailingEmbedder struct{}

func (TestFailingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, fmt.Errorf("injected test embedder failure")
}
