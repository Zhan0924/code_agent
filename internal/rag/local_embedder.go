// Package rag - local_embedder.go implements a local hash-based embedder that
// generates deterministic vector embeddings without any external API dependency.
//
// This uses the "random projection / hashing trick" approach:
//   - Tokenize text into words
//   - Hash each word to a deterministic seed
//   - Use that seed to generate a random unit vector contribution
//   - Sum all word contributions and L2-normalize
//
// Quality: sufficient for keyword-level and basic semantic similarity.
// NOT as good as neural embeddings (text-embedding-3-small, etc.) but works
// offline with zero latency and zero cost. Ideal for dev/testing or as fallback.
package rag

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"

	"go.uber.org/zap"
)

// LocalHashEmbedder generates deterministic embeddings using hash-based random projections.
// It implements the Embedder interface without requiring any external API.
type LocalHashEmbedder struct {
	dim    int
	logger *zap.Logger
}

// NewLocalHashEmbedder creates a local embedder that produces vectors of the given dimension.
func NewLocalHashEmbedder(dim int, logger *zap.Logger) *LocalHashEmbedder {
	logger.Info("using local hash embedder (no external API required)",
		zap.Int("vector_dim", dim),
	)
	return &LocalHashEmbedder{
		dim:    dim,
		logger: logger.With(zap.String("component", "local-embedder")),
	}
}

// Embed generates vector embeddings for the given texts using hash-based random projections.
func (e *LocalHashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i, text := range texts {
		result[i] = e.embedSingle(text)
	}
	return result, nil
}

// embedSingle generates a single embedding vector for a text string.
func (e *LocalHashEmbedder) embedSingle(text string) []float32 {
	vec := make([]float64, e.dim)

	// Tokenize: split into words, lowercase, remove punctuation
	tokens := tokenize(text)

	if len(tokens) == 0 {
		// Return zero vector for empty text
		out := make([]float32, e.dim)
		return out
	}

	// Also generate bigrams for slightly better semantic capture
	allTokens := make([]string, 0, len(tokens)+len(tokens)-1)
	allTokens = append(allTokens, tokens...)
	for i := 0; i < len(tokens)-1; i++ {
		allTokens = append(allTokens, tokens[i]+"_"+tokens[i+1])
	}

	// Hash each token and project into the vector space
	for _, token := range allTokens {
		h := fnv.New64a()
		h.Write([]byte(token))
		seed := h.Sum64()

		// Use the hash to deterministically pick dimensions and signs
		// Each token contributes to ~sqrt(dim) random dimensions
		numDims := int(math.Sqrt(float64(e.dim))) + 1
		for j := 0; j < numDims; j++ {
			// Linear congruential generator from the seed
			seed = seed*6364136223846793005 + 1442695040888963407
			idx := int(seed % uint64(e.dim))
			sign := float64(1)
			if (seed>>32)&1 == 0 {
				sign = -1
			}
			vec[idx] += sign
		}
	}

	// L2 normalize
	return l2Normalize(vec)
}

// tokenize splits text into lowercase word tokens, filtering out short/stop words.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})

	// Filter: keep tokens with length >= 2
	var tokens []string
	for _, w := range words {
		if len(w) >= 2 {
			tokens = append(tokens, w)
		}
	}
	return tokens
}

// l2Normalize normalizes a float64 vector to unit length and converts to float32.
func l2Normalize(vec []float64) []float32 {
	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)

	out := make([]float32, len(vec))
	if norm > 0 {
		for i, v := range vec {
			out[i] = float32(v / norm)
		}
	}
	return out
}
