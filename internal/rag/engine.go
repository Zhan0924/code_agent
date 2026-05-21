// Package rag implements the deep code semantic Retrieval-Augmented Generation engine.
// It provides AST-aware code parsing, dual-recall (dense + sparse) retrieval,
// and cross-encoder reranking for high-precision code search.
package rag

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// Embedder defines the interface for generating text embeddings.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// VectorStore defines the interface for vector storage and retrieval.
type VectorStore interface {
	Upsert(ctx context.Context, chunks []models.CodeChunk) error
	SearchDense(ctx context.Context, vector []float32, topK int, filters map[string]string) ([]models.RetrievalResult, error)
	SearchSparse(ctx context.Context, query string, topK int, filters map[string]string) ([]models.RetrievalResult, error)
	Delete(ctx context.Context, ids []string) error
}

// Reranker defines the interface for cross-encoder reranking.
type Reranker interface {
	Rerank(ctx context.Context, query string, results []models.RetrievalResult, topN int) ([]models.RetrievalResult, error)
}

// Engine is the main RAG engine that orchestrates parsing, indexing, and retrieval.
type Engine struct {
	embedder Embedder
	store    VectorStore
	reranker Reranker
	depGraph *DepGraph
	cfg      *config.RAGConfig
	logger   *zap.Logger
}

// NewEngine creates a new RAG engine with the given components.
func NewEngine(embedder Embedder, store VectorStore, reranker Reranker, cfg *config.RAGConfig, logger *zap.Logger) *Engine {
	return &Engine{
		embedder: embedder,
		store:    store,
		reranker: reranker,
		depGraph: NewDepGraph(),
		cfg:      cfg,
		logger:   logger.With(zap.String("component", "rag")),
	}
}

// IndexCode parses and indexes code files into the vector store.
func (e *Engine) IndexCode(ctx context.Context, filePath, language, content string, metadata map[string]string) error {
	// Parse code into semantic chunks using AST-aware parsing
	chunks := e.parseCodeChunks(filePath, language, content, metadata)

	if len(chunks) == 0 {
		e.logger.Warn("no chunks generated from file", zap.String("file", filePath))
		return nil
	}

	// Generate embeddings for all chunks
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		// Create enriched text for embedding: include symbol name and type for better semantic matching
		texts[i] = e.buildEmbeddingText(c)
	}

	embeddings, err := e.embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("failed to generate embeddings: %w", err)
	}

	for i := range chunks {
		chunks[i].Embedding = embeddings[i]
	}

	// Store in vector database
	if err := e.store.Upsert(ctx, chunks); err != nil {
		return fmt.Errorf("failed to upsert chunks: %w", err)
	}

	// Populate dependency graph for Go files
	if language == "go" {
		e.depGraph.RemoveFile(filePath)
		depInfo := ExtractGoDeps(filePath, content)
		PopulateDepGraph(e.depGraph, depInfo)
	}

	e.logger.Info("code indexed",
		zap.String("file", filePath),
		zap.Int("chunks", len(chunks)),
	)

	return nil
}

// DepGraph returns the engine's dependency graph for external queries.
func (e *Engine) DepGraph() *DepGraph {
	return e.depGraph
}

// Retrieve performs dual-recall retrieval followed by reranking.
func (e *Engine) Retrieve(ctx context.Context, query string, filters map[string]string) ([]models.RetrievalResult, error) {
	// Generate query embedding for dense retrieval
	embeddings, err := e.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("failed to embed query: %w", err)
	}

	topK := e.cfg.TopK

	// Dual-recall: dense + sparse search in parallel
	type searchResult struct {
		results []models.RetrievalResult
		err     error
		source  string
	}
	resultCh := make(chan searchResult, 2)

	// Dense vector search (semantic)
	go func() {
		results, err := e.store.SearchDense(ctx, embeddings[0], topK, filters)
		resultCh <- searchResult{results: results, err: err, source: "dense"}
	}()

	// Sparse search (BM25 - exact variable name matching)
	go func() {
		results, err := e.store.SearchSparse(ctx, query, topK, filters)
		resultCh <- searchResult{results: results, err: err, source: "sparse"}
	}()

	// Collect results from both paths
	var allResults []models.RetrievalResult
	for i := 0; i < 2; i++ {
		r := <-resultCh
		if r.err != nil {
			e.logger.Warn("search path failed", zap.String("source", r.source), zap.Error(r.err))
			continue
		}
		for j := range r.results {
			r.results[j].Source = r.source
		}
		allResults = append(allResults, r.results...)
	}

	// Deduplicate by chunk ID
	allResults = deduplicateResults(allResults)

	if len(allResults) == 0 {
		return nil, nil
	}

	// Rerank if enabled and reranker is available
	if e.cfg.RerankEnabled && e.reranker != nil {
		reranked, err := e.reranker.Rerank(ctx, query, allResults, e.cfg.RerankTopN)
		if err != nil {
			e.logger.Warn("reranking failed, using raw results", zap.Error(err))
		} else {
			allResults = reranked
		}
	}

	// Sort by score descending and limit
	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].Score > allResults[j].Score
	})

	if len(allResults) > e.cfg.RerankTopN {
		allResults = allResults[:e.cfg.RerankTopN]
	}

	// Dependency-aware expansion: boost results whose symbols are connected
	// to already-retrieved symbols in the dependency graph.
	allResults = e.expandWithDeps(allResults)

	return allResults, nil
}

// parseCodeChunks splits code into semantic chunks using AST-aware parsing.
// It attempts to identify function boundaries, class definitions, and other
// meaningful code structures rather than doing naive text splitting.
func (e *Engine) parseCodeChunks(filePath, language, content string, metadata map[string]string) []models.CodeChunk {
	var chunks []models.CodeChunk

	// Try AST-based parsing first
	astChunks := parseWithAST(language, content)
	if len(astChunks) > 0 {
		for _, ac := range astChunks {
			chunk := models.CodeChunk{
				ID:           fmt.Sprintf("%s:%s:%d", filePath, ac.symbolName, ac.startLine),
				FilePath:     filePath,
				Language:     language,
				SymbolName:   ac.symbolName,
				SymbolType:   ac.symbolType,
				Content:      ac.content,
				StartLine:    ac.startLine,
				EndLine:      ac.endLine,
				Dependencies: ac.dependencies,
				Metadata:     metadata,
			}
			chunks = append(chunks, chunk)
		}
		return chunks
	}

	// Fallback: sliding window text chunking
	lines := strings.Split(content, "\n")
	chunkSize := e.cfg.ChunkMaxTokens * 4 // approximate chars per token
	overlap := e.cfg.OverlapTokens * 4

	text := content
	for i := 0; i < len(text); i += chunkSize - overlap {
		end := i + chunkSize
		if end > len(text) {
			end = len(text)
		}

		chunkContent := text[i:end]
		startLine := countLines(text[:i]) + 1
		endLine := startLine + countLines(chunkContent)

		chunk := models.CodeChunk{
			ID:        fmt.Sprintf("%s:chunk:%d", filePath, i),
			FilePath:  filePath,
			Language:  language,
			Content:   chunkContent,
			StartLine: startLine,
			EndLine:   endLine,
			Metadata:  metadata,
		}
		chunks = append(chunks, chunk)

		if end >= len(text) {
			break
		}
	}

	_ = lines // suppress unused warning
	return chunks
}

// buildEmbeddingText creates an enriched text representation for embedding.
// For markdown documents, it includes the heading hierarchy path for better
// semantic matching (e.g., "Architecture > Deployment > HA Design").
func (e *Engine) buildEmbeddingText(chunk models.CodeChunk) string {
	var parts []string

	if chunk.SymbolType != "" && chunk.SymbolName != "" {
		parts = append(parts, fmt.Sprintf("%s %s", chunk.SymbolType, chunk.SymbolName))
	}
	if chunk.FilePath != "" {
		parts = append(parts, fmt.Sprintf("file: %s", chunk.FilePath))
	}

	// For markdown chunks, include heading hierarchy path for richer context
	for _, dep := range chunk.Dependencies {
		if strings.HasPrefix(dep, "path:") {
			headingPath := strings.TrimPrefix(dep, "path:")
			parts = append(parts, fmt.Sprintf("section: %s", headingPath))
		}
		if strings.HasPrefix(dep, "lang:") {
			lang := strings.TrimPrefix(dep, "lang:")
			parts = append(parts, fmt.Sprintf("contains %s code", lang))
		}
	}

	parts = append(parts, chunk.Content)

	return strings.Join(parts, "\n")
}

// deduplicateResults removes duplicate retrieval results based on chunk ID.
func deduplicateResults(results []models.RetrievalResult) []models.RetrievalResult {
	seen := make(map[string]bool)
	var unique []models.RetrievalResult

	for _, r := range results {
		if !seen[r.Chunk.ID] {
			seen[r.Chunk.ID] = true
			unique = append(unique, r)
		}
	}
	return unique
}

// countLines counts the number of newline characters in a string.
func countLines(s string) int {
	count := 0
	for _, c := range s {
		if c == '\n' {
			count++
		}
	}
	return count
}

// expandWithDeps uses the dependency graph to expand retrieval results.
// For each retrieved symbol, it follows dependency edges to find related
// symbols and boosts their scores if they appear in the result set.
func (e *Engine) expandWithDeps(results []models.RetrievalResult) []models.RetrievalResult {
	if len(results) == 0 {
		return results
	}

	// Extract seed symbols from top results
	seeds := make([]string, 0, len(results))
	for _, r := range results {
		if r.Chunk.SymbolName != "" {
			qSym := qualifiedSymbol(r.Chunk.FilePath, r.Chunk.SymbolName)
			seeds = append(seeds, qSym)
		}
	}

	if len(seeds) == 0 {
		return results
	}

	// Expand via dependency graph (1 hop)
	expanded := e.depGraph.ExpandRetrievalContext(seeds, 1)

	// Build a boost map: symbol → boost score
	boostMap := make(map[string]float64, len(expanded))
	for _, s := range expanded {
		boostMap[s.Symbol] = s.Score
	}

	// Apply boosts to existing results
	for i := range results {
		qSym := qualifiedSymbol(results[i].Chunk.FilePath, results[i].Chunk.SymbolName)
		if boost, ok := boostMap[qSym]; ok {
			results[i].Score += boost * 0.3 // 30% weight for dependency boost
		}
	}

	// Re-sort after boosting
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}
