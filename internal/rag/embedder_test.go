package rag

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap"
)

// TestOpenAIEmbedder_UsesInjectedHTTPClient mirrors TestAPIReranker_UsesInjectedHTTPClient
// for the embedder path: confirms that the *http.Client supplied at construction
// (the wiring point for the egress-protected client) is propagated through
// openai.ClientConfig.HTTPClient and used for every API call.
func TestOpenAIEmbedder_UsesInjectedHTTPClient(t *testing.T) {
	rt := &stubRoundTripper{err: errors.New("egress denied")}
	httpClient := &http.Client{Transport: rt}

	ragCfg := &config.RAGConfig{
		EmbeddingAPIKey:  "test-key",
		EmbeddingBaseURL: "https://denied.example.com",
		EmbeddingModel:   "text-embedding-test",
	}
	emb := NewOpenAIEmbedder(ragCfg, nil, httpClient, zap.NewNop())

	_, err := emb.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatalf("expected error from stub transport, got nil")
	}
	if rt.hits == 0 {
		t.Fatalf("expected injected transport to be hit at least once, got 0")
	}
}

// TestOpenAIEmbedder_NilClientLeavesDefault verifies that passing nil leaves the
// go-openai SDK on its default transport; the constructor must not panic and
// must produce a usable embedder.
func TestOpenAIEmbedder_NilClientLeavesDefault(t *testing.T) {
	ragCfg := &config.RAGConfig{EmbeddingAPIKey: "k", EmbeddingBaseURL: "https://example.com", EmbeddingModel: "m"}
	emb := NewOpenAIEmbedder(ragCfg, nil, nil, zap.NewNop())
	if emb == nil || emb.client == nil {
		t.Fatal("expected non-nil embedder with default openai client")
	}
}
