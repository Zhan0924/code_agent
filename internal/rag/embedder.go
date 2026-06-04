// Package rag - embedder.go implements the Embedder interface using OpenAI-compatible
// embedding APIs. This resolves F2: the RAG engine previously passed nil for embedder,
// causing Retrieve() to panic on nil pointer dereference.
package rag

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agent/code_agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"
)

// OpenAIEmbedder implements the Embedder interface using OpenAI's embedding API.
// It supports any OpenAI-compatible endpoint (OpenAI, Azure, local vLLM, Ollama, etc.)
type OpenAIEmbedder struct {
	client *openai.Client
	model  string
	logger *zap.Logger
}

// NewOpenAIEmbedder creates a new embedder using dedicated RAG embedding credentials.
// It reads embedding_base_url and embedding_api_key from RAGConfig, falling back
// to LLM primary config if RAG-specific credentials are not set.
//
// httpClient should be the process-wide egress-protected client built by
// security.NewEgressHTTPClient so that embedding traffic is subject to the same
// SSRF / allow-list policy as LLM and MCP traffic. Passing nil leaves the
// go-openai SDK on its default transport (kept for tests and backward-compat).
func NewOpenAIEmbedder(ragCfg *config.RAGConfig, llmFallback *config.LLMProviderConfig, httpClient *http.Client, logger *zap.Logger) *OpenAIEmbedder {
	// Determine API key: prefer RAG-specific, fallback to LLM primary
	apiKey := ragCfg.EmbeddingAPIKey
	if apiKey == "" && llmFallback != nil {
		apiKey = llmFallback.APIKey
	}

	// Determine base URL: prefer RAG-specific, fallback to LLM primary
	baseURL := ragCfg.EmbeddingBaseURL
	if baseURL == "" && llmFallback != nil {
		baseURL = llmFallback.BaseURL
	}

	clientCfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		clientCfg.BaseURL = baseURL
	}
	if httpClient != nil {
		clientCfg.HTTPClient = httpClient
	}

	model := ragCfg.EmbeddingModel
	if model == "" {
		model = "text-embedding-3-small"
	}

	logger.Info("embedding client configured",
		zap.String("model", model),
		zap.String("base_url", baseURL),
		zap.Bool("has_api_key", apiKey != ""),
		zap.Bool("using_rag_credentials", ragCfg.EmbeddingAPIKey != ""),
		zap.Bool("egress_client", httpClient != nil),
	)

	return &OpenAIEmbedder{
		client: openai.NewClientWithConfig(clientCfg),
		model:  model,
		logger: logger.With(zap.String("component", "embedder"), zap.String("model", model)),
	}
}

// Embed generates vector embeddings for the given texts using batch API calls.
// It handles batching automatically for large input sets.
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	const maxBatchSize = 10 // DashScope batch limit (text-embedding-v4)
	var allEmbeddings [][]float32

	for i := 0; i < len(texts); i += maxBatchSize {
		end := i + maxBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		// Convert to EmbeddingRequestStrings
		req := openai.EmbeddingRequest{
			Input:      batch,
			Model:      openai.EmbeddingModel(e.model),
			Dimensions: 1536, // Match pgvector column size
		}

		resp, err := e.client.CreateEmbeddings(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("embedding API call failed (batch %d-%d): %w", i, end, err)
		}

		for _, item := range resp.Data {
			allEmbeddings = append(allEmbeddings, item.Embedding)
		}

		e.logger.Debug("embedding batch completed",
			zap.Int("batch_start", i),
			zap.Int("batch_end", end),
			zap.Int("tokens_used", resp.Usage.TotalTokens),
		)
	}

	if len(allEmbeddings) != len(texts) {
		return nil, fmt.Errorf("embedding count mismatch: got %d, expected %d", len(allEmbeddings), len(texts))
	}

	return allEmbeddings, nil
}
