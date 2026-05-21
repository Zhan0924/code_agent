// Package rag - reranker.go implements the Reranker interface using
// OpenAI-compatible rerank APIs (DashScope, Cohere, Jina, etc.).
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么要 rerank】
//
//	双路召回（dense+sparse）的 topK 是召回结果，不等于"最相关"的 K 个。
//	Dense 可能因为向量模型维度受限把语义相近但目的不同的代码混在一起；
//	Sparse 可能因为关键词完全匹配但上下文完全跑题。
//	cross-encoder reranker 对 (query, candidate) 对做精排：
//	  · 输入是整个 query + candidate，不需要共享嵌入空间；
//	  · 能捕捉 query 中"负面线索"——如"not auth but logging"不会让 auth 排前。
//	经验上 dense+BM25 召回 top-30，再 rerank 取 top-3，质量显著优于直接 top-3。
//
// 【为什么用 OpenAI 兼容接口】
//
//	DashScope / Cohere / Jina 的 rerank API 约定大同小异，都是
//	`POST /rerank {query, documents, top_n}`。我们统一按 Jina 格式发请求，
//	90% 兼容。**失败 fall-through**：rerank 接口挂了，直接把 dual-recall
//	的结果按 score 排序返回，保证可用性。
//
// ============================================================================
package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// APIReranker implements the Reranker interface using an HTTP rerank endpoint.
type APIReranker struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
	logger  *zap.Logger
}

// NewAPIReranker creates a reranker backed by an HTTP API.
func NewAPIReranker(cfg *config.RAGConfig, logger *zap.Logger) *APIReranker {
	logger.Info("reranker initialized",
		zap.String("model", cfg.RerankModel),
		zap.String("base_url", cfg.RerankBaseURL),
	)
	return &APIReranker{
		baseURL: cfg.RerankBaseURL,
		apiKey:  cfg.RerankAPIKey,
		model:   cfg.RerankModel,
		client:  &http.Client{Timeout: 30 * time.Second},
		logger:  logger.With(zap.String("component", "reranker")),
	}
}

// rerankRequest is the request body for the rerank API.
type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

// rerankResponse is the response from the rerank API.
type rerankResponse struct {
	Results []rerankResult `json:"results"`
}

type rerankResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

// Rerank reranks the given retrieval results using the cross-encoder API.
func (r *APIReranker) Rerank(ctx context.Context, query string, results []models.RetrievalResult, topN int) ([]models.RetrievalResult, error) {
	if len(results) == 0 {
		return nil, nil
	}

	// Extract document texts
	documents := make([]string, len(results))
	for i, res := range results {
		documents[i] = res.Chunk.Content
	}

	reqBody := rerankRequest{
		Model:     r.model,
		Query:     query,
		Documents: documents,
		TopN:      topN,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal rerank request: %w", err)
	}

	url := r.baseURL + "/reranks"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create rerank request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read rerank response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var rerankResp rerankResponse
	if err := json.Unmarshal(respBody, &rerankResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal rerank response: %w", err)
	}

	// Map scores back to results
	reranked := make([]models.RetrievalResult, 0, len(rerankResp.Results))
	for _, rr := range rerankResp.Results {
		if rr.Index >= 0 && rr.Index < len(results) {
			res := results[rr.Index]
			res.Score = rr.RelevanceScore
			res.Source = "reranked"
			reranked = append(reranked, res)
		}
	}

	// Sort by score descending
	sort.Slice(reranked, func(i, j int) bool {
		return reranked[i].Score > reranked[j].Score
	})

	// Limit to topN
	if len(reranked) > topN {
		reranked = reranked[:topN]
	}

	r.logger.Debug("reranking complete",
		zap.Int("input_docs", len(documents)),
		zap.Int("output_docs", len(reranked)),
	)

	return reranked, nil
}
