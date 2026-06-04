package rag

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// stubRoundTripper records that it was hit and refuses every request, simulating
// the behaviour of an egress-protected transport that has denied the target.
type stubRoundTripper struct {
	hits int
	err  error
}

func (s *stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	s.hits++
	return nil, s.err
}

// TestAPIReranker_UsesInjectedHTTPClient ensures that when a custom *http.Client
// is passed (the wiring point for the egress-protected client built in
// cmd/agent/main.go), all reranker calls are dispatched through that client and
// never fall back to a default transport.
func TestAPIReranker_UsesInjectedHTTPClient(t *testing.T) {
	rt := &stubRoundTripper{err: errors.New("egress denied")}
	httpClient := &http.Client{Transport: rt}

	cfg := &config.RAGConfig{
		RerankBaseURL: "https://denied.example.com",
		RerankAPIKey:  "test-key",
		RerankModel:   "test-rerank-v1",
	}
	r := NewAPIReranker(cfg, httpClient, zap.NewNop())

	results := []models.RetrievalResult{{Chunk: models.CodeChunk{Content: "hello"}}}
	_, err := r.Rerank(context.Background(), "any query", results, 1)
	if err == nil {
		t.Fatalf("expected error from stub transport, got nil")
	}
	if rt.hits != 1 {
		t.Fatalf("expected 1 hit on injected transport, got %d", rt.hits)
	}
}

// TestAPIReranker_NilClientFallback verifies the nil-client backward-compat
// branch: a plain http.Client with a finite timeout is used so the reranker
// stays callable from tests that don't wire an egress client.
func TestAPIReranker_NilClientFallback(t *testing.T) {
	cfg := &config.RAGConfig{RerankBaseURL: "https://example.com", RerankModel: "m"}
	r := NewAPIReranker(cfg, nil, zap.NewNop())
	if r.client == nil {
		t.Fatal("expected fallback http.Client, got nil")
	}
	if r.client.Timeout == 0 {
		t.Fatal("expected fallback client to have non-zero timeout")
	}
}
