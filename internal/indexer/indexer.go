// Package indexer implements a Git repository scanning and incremental indexing pipeline.
// (F9) It walks a repository, parses code into AST-aware chunks, generates embeddings,
// and upserts them into the vector store for RAG retrieval.
package indexer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/rag"
	"go.uber.org/zap"
)

// supportedExtensions maps file extensions to language names for AST parsing.
var supportedExtensions = map[string]string{
	".go":   "go",
	".py":   "python",
	".js":   "javascript",
	".ts":   "typescript",
	".java": "java",
	".rs":   "rust",
	".rb":   "ruby",
	".c":    "c",
	".cpp":  "cpp",
	".h":    "c",
	".hpp":  "cpp",
	".md":   "markdown",
	".sh":   "shell",
	".bash": "bash",
	".yaml": "yaml",
	".yml":  "yaml",
	".json": "json",
	".toml": "toml",
	".txt":  "text",
}

// defaultIgnorePatterns are directories/files to skip during indexing.
var defaultIgnorePatterns = []string{
	".git", "node_modules", "vendor", "__pycache__", ".venv",
	"dist", "build", "target", "bin", ".idea", ".vscode",
}

// FileChecksum stores the content hash of a file for incremental indexing.
type FileChecksum struct {
	Path      string    `json:"path"`
	Hash      string    `json:"hash"`
	IndexedAt time.Time `json:"indexed_at"`
}

// IndexStats tracks indexing progress and results.
type IndexStats struct {
	TotalFiles   int           `json:"total_files"`
	IndexedFiles int           `json:"indexed_files"`
	SkippedFiles int           `json:"skipped_files"`
	TotalChunks  int           `json:"total_chunks"`
	Duration     time.Duration `json:"duration"`
	Errors       []string      `json:"errors,omitempty"`
}

// Indexer scans repositories and indexes code into the RAG engine.
type Indexer struct {
	ragEngine *rag.Engine
	cfg       *config.RAGConfig
	logger    *zap.Logger

	// checksums stores file hashes for incremental indexing.
	// In production, this would be backed by a persistent store.
	checksumMu sync.RWMutex
	checksums  map[string]FileChecksum
}

// NewIndexer creates a new code repository indexer.
func NewIndexer(ragEngine *rag.Engine, cfg *config.RAGConfig, logger *zap.Logger) *Indexer {
	return &Indexer{
		ragEngine: ragEngine,
		cfg:       cfg,
		logger:    logger.With(zap.String("component", "indexer")),
		checksums: make(map[string]FileChecksum),
	}
}

// IndexRepository scans a directory tree, parses code files, generates embeddings,
// and upserts the resulting chunks into the vector store.
// It supports incremental indexing — only changed files are re-processed.
func (idx *Indexer) IndexRepository(ctx context.Context, repoPath string, projectName string) (*IndexStats, error) {
	start := time.Now()
	stats := &IndexStats{}

	idx.logger.Info("starting repository indexing",
		zap.String("path", repoPath),
		zap.String("project", projectName),
	)

	// Collect files to index
	var filesToIndex []string
	err := filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip inaccessible files
		}

		// Skip ignored directories
		if info.IsDir() {
			for _, pattern := range defaultIgnorePatterns {
				if info.Name() == pattern {
					return filepath.SkipDir
				}
			}
			return nil
		}

		// Check if file extension is supported
		ext := strings.ToLower(filepath.Ext(path))
		if _, ok := supportedExtensions[ext]; !ok {
			stats.SkippedFiles++
			return nil
		}

		// Skip large files (> 1MB)
		if info.Size() > 1024*1024 {
			stats.SkippedFiles++
			return nil
		}

		stats.TotalFiles++
		filesToIndex = append(filesToIndex, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk repository: %w", err)
	}

	idx.logger.Info("files discovered",
		zap.Int("total", stats.TotalFiles),
		zap.Int("skipped", stats.SkippedFiles),
	)

	// Process files with bounded concurrency
	const maxConcurrency = 8
	sem := make(chan struct{}, maxConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, filePath := range filesToIndex {
		select {
		case <-ctx.Done():
			break
		default:
		}

		wg.Add(1)
		sem <- struct{}{} // Acquire semaphore

		go func(fp string) {
			defer wg.Done()
			defer func() { <-sem }() // Release semaphore

			relPath, _ := filepath.Rel(repoPath, fp)

			// [OPT-17] Read file once and check hash in-memory to avoid double I/O.
			content, err := os.ReadFile(fp)
			if err != nil {
				mu.Lock()
				stats.Errors = append(stats.Errors, fmt.Sprintf("read %s: %v", relPath, err))
				mu.Unlock()
				return
			}

			// Check if file has changed using the already-read content
			if !idx.hasContentChanged(fp, content) {
				mu.Lock()
				stats.SkippedFiles++
				mu.Unlock()
				return
			}

			// Detect language from extension
			ext := strings.ToLower(filepath.Ext(fp))
			lang := supportedExtensions[ext]

			// Index through RAG engine (signature: filePath, language, content, metadata)
			err = idx.ragEngine.IndexCode(ctx, relPath, lang, string(content), map[string]string{
				"project": projectName,
			})
			if err != nil {
				mu.Lock()
				stats.Errors = append(stats.Errors, fmt.Sprintf("index %s: %v", relPath, err))
				mu.Unlock()
				return
			}

			// Update checksum cache
			idx.updateChecksum(fp, content)

			mu.Lock()
			stats.IndexedFiles++
			mu.Unlock()

			idx.logger.Debug("file indexed", zap.String("file", relPath), zap.String("lang", lang))
		}(filePath)
	}

	wg.Wait()

	stats.Duration = time.Since(start)

	idx.logger.Info("repository indexing complete",
		zap.Int("indexed", stats.IndexedFiles),
		zap.Int("skipped", stats.SkippedFiles),
		zap.Int("errors", len(stats.Errors)),
		zap.Duration("duration", stats.Duration),
	)

	return stats, nil
}

// hasContentChanged checks if in-memory content differs from cached hash.
// [OPT-17] This avoids the double file read of the old hasFileChanged.
func (idx *Indexer) hasContentChanged(filePath string, content []byte) bool {
	hash := fmt.Sprintf("%x", sha256.Sum256(content))

	idx.checksumMu.RLock()
	existing, ok := idx.checksums[filePath]
	idx.checksumMu.RUnlock()

	return !ok || existing.Hash != hash
}

// updateChecksum stores the current file hash in the cache.
func (idx *Indexer) updateChecksum(filePath string, content []byte) {
	hash := fmt.Sprintf("%x", sha256.Sum256(content))

	idx.checksumMu.Lock()
	idx.checksums[filePath] = FileChecksum{
		Path:      filePath,
		Hash:      hash,
		IndexedAt: time.Now(),
	}
	idx.checksumMu.Unlock()
}

// IndexRepositoryAny wraps IndexRepository to satisfy the api.Indexer interface
// which returns (interface{}, error) to avoid circular imports.
func (idx *Indexer) IndexRepositoryAny(ctx context.Context, repoPath, projectName string) (interface{}, error) {
	return idx.IndexRepository(ctx, repoPath, projectName)
}

// GetStats returns the current checksums count for monitoring.
func (idx *Indexer) GetStats() map[string]int {
	idx.checksumMu.RLock()
	defer idx.checksumMu.RUnlock()
	return map[string]int{
		"indexed_files": len(idx.checksums),
	}
}
