// Package rag / bm25.go — in-process BM25 sparse retrieval.
//
// Why an in-process index rather than Qdrant's native full-text search:
// Qdrant ≥ 1.8 supports full-text indices, but the codebase's existing
// qdrant-go client build does not expose them in a version-stable way, and
// the previous SearchSparse stub did substring matching on symbol_name with
// a hardcoded score of 0.5 — not usable for retrieval ranking. This file
// provides a standalone BM25 index that scores all chunks over the query.
//
// Scale: fine for corpora up to ~100k chunks (dozens of MB RAM, sub-10ms
// queries). Beyond that, move to Qdrant's sparse-vector feature or an
// external keyword engine (Meilisearch / Elasticsearch). This is documented
// on BM25Index so the tradeoff is visible to future maintainers.
package rag

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/agent/code_agent/internal/models"
)

// BM25 tuning constants match Lucene's defaults. b=0.75 gives moderate
// length normalization; k1=1.2 is standard for short-to-medium docs.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// bm25Doc is one indexed chunk, pre-tokenised with per-term frequency.
type bm25Doc struct {
	chunk  models.CodeChunk
	length int            // total tokens in the doc (for length normalization)
	tf     map[string]int // term → count in this doc
}

// BM25Index is an in-memory inverted-ish index that scores query strings
// against a corpus of CodeChunks. Build() rebuilds from scratch; Search()
// is read-only and safe to call concurrently.
//
// Scale caveat: designed for ≤100k chunks in-process. For larger corpora,
// switch to Qdrant sparse vectors or a dedicated keyword engine.
type BM25Index struct {
	mu      sync.RWMutex
	docs    []bm25Doc
	df      map[string]int // term → number of docs containing it
	avgLen  float64
	numDocs int
}

// NewBM25Index creates an empty index. Call Build before Search.
func NewBM25Index() *BM25Index {
	return &BM25Index{df: map[string]int{}}
}

// Build replaces the index content with the given chunks. O(N * avg tokens).
// Call whenever the underlying corpus changes materially — callers that
// index continuously should batch rebuilds (e.g. every N upserts or on a
// TTL) rather than rebuilding per-chunk.
func (b *BM25Index) Build(chunks []models.CodeChunk) {
	docs := make([]bm25Doc, 0, len(chunks))
	df := make(map[string]int, 1024)
	totalLen := 0
	for _, c := range chunks {
		tokens := tokenizeForBM25(c.Content)
		if len(tokens) == 0 {
			// Still index on the symbol name if content tokenises empty
			// (e.g. binary/decoded payload). Keeps short chunks reachable.
			tokens = tokenizeForBM25(c.SymbolName)
			if len(tokens) == 0 {
				continue
			}
		}
		tf := make(map[string]int, len(tokens))
		seen := make(map[string]struct{}, len(tokens))
		for _, t := range tokens {
			tf[t]++
			if _, ok := seen[t]; !ok {
				seen[t] = struct{}{}
				df[t]++
			}
		}
		docs = append(docs, bm25Doc{chunk: c, length: len(tokens), tf: tf})
		totalLen += len(tokens)
	}
	avg := 0.0
	if len(docs) > 0 {
		avg = float64(totalLen) / float64(len(docs))
	}

	b.mu.Lock()
	b.docs = docs
	b.df = df
	b.numDocs = len(docs)
	b.avgLen = avg
	b.mu.Unlock()
}

// Size returns the current document count. Used by callers that want to
// skip sparse search entirely when the index is empty (first query after
// process start).
func (b *BM25Index) Size() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.numDocs
}

// Search returns the top-K chunks by BM25 score for the query. Returns an
// empty slice (not nil) if the index is empty or the query has no matching
// tokens; callers can pass that straight through to Retrieve without a
// nil-check.
func (b *BM25Index) Search(query string, topK int) []models.RetrievalResult {
	if topK <= 0 {
		topK = 10
	}
	qTerms := tokenizeForBM25(query)
	if len(qTerms) == 0 {
		return []models.RetrievalResult{}
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.numDocs == 0 {
		return []models.RetrievalResult{}
	}

	// Unique-ify query terms but remember no duplicates — duplicated query
	// tokens should not amplify their own score (standard BM25 practice).
	unique := make(map[string]struct{}, len(qTerms))
	for _, t := range qTerms {
		unique[t] = struct{}{}
	}

	type scoredLocal = scored
	heap := make([]scoredLocal, 0, topK)

	for i, d := range b.docs {
		var score float64
		for term := range unique {
			tf, ok := d.tf[term]
			if !ok {
				continue
			}
			idf := bm25IDF(b.numDocs, b.df[term])
			if idf <= 0 {
				continue // term in every doc → no discriminative power
			}
			norm := 1 - bm25B + bm25B*(float64(d.length)/b.avgLen)
			score += idf * (float64(tf) * (bm25K1 + 1)) /
				(float64(tf) + bm25K1*norm)
		}
		if score <= 0 {
			continue
		}
		heap = insertTopK(heap, scored{docIdx: i, score: score}, topK)
	}

	// heap is now at most topK entries, ordered by ascending score (from the
	// min-heap maintenance below). Sort descending for return.
	sort.Slice(heap, func(i, j int) bool { return heap[i].score > heap[j].score })

	results := make([]models.RetrievalResult, 0, len(heap))
	for _, s := range heap {
		results = append(results, models.RetrievalResult{
			Chunk:  b.docs[s.docIdx].chunk,
			Score:  s.score,
			Source: "sparse",
		})
	}
	return results
}

// bm25IDF computes the Robertson-Sparck Jones IDF variant used by BM25.
// Returns 0 (not negative) when a term is too common, since negative IDF
// would let "anti-match" documents beat true hits.
func bm25IDF(numDocs, df int) float64 {
	if df <= 0 {
		return 0
	}
	n := float64(numDocs)
	d := float64(df)
	v := math.Log((n - d + 0.5) / (d + 0.5))
	if v <= 0 {
		return 0
	}
	return v
}

// insertTopK maintains a bounded min-heap semantics using a sorted slice.
// At ≤ topK entries this is cheap; for large topK a real container/heap
// would win but the common case here is K ≤ 20 so the constant factor is
// irrelevant.
func insertTopK(heap []scored, item scored, k int) []scored {
	if len(heap) < k {
		heap = append(heap, item)
		// Keep sorted ascending so heap[0] is always the min we might evict.
		sort.Slice(heap, func(i, j int) bool { return heap[i].score < heap[j].score })
		return heap
	}
	if item.score <= heap[0].score {
		return heap
	}
	heap[0] = item
	sort.Slice(heap, func(i, j int) bool { return heap[i].score < heap[j].score })
	return heap
}

type scored struct {
	docIdx int
	score  float64
}

// tokenizeForBM25 splits text into lowercased alphanumeric tokens and
// additionally splits camelCase/snake_case identifiers so that searching
// for "http client" matches "HTTPClient" or "http_client". Common English
// and code-comment stopwords are dropped to cut index size and noise.
func tokenizeForBM25(text string) []string {
	if text == "" {
		return nil
	}
	// First pass: split on any non-alphanumeric rune.
	raw := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(raw)*2)
	for _, word := range raw {
		// Split camelCase and preserve both the combined and sub-token
		// forms so exact-identifier queries still hit.
		for _, sub := range splitCamel(word) {
			s := strings.ToLower(sub)
			if len(s) < 2 {
				continue
			}
			if _, skip := bm25Stopwords[s]; skip {
				continue
			}
			out = append(out, s)
		}
	}
	return out
}

// splitCamel breaks "HTTPClient" into ["HTTP", "Client"] and "parseJSON"
// into ["parse", "JSON"]. The original combined word is also kept so that
// querying for the full identifier lands an exact TF hit.
func splitCamel(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{s}
	runes := []rune(s)
	start := 0
	for i := 1; i < len(runes); i++ {
		prev := runes[i-1]
		cur := runes[i]
		// Boundary: lower→upper (fooBar), or upper→upper-then-lower (HTTPClient → HTTP, Client).
		boundary := false
		if unicode.IsLower(prev) && unicode.IsUpper(cur) {
			boundary = true
		} else if unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
			boundary = true
		}
		if boundary {
			if i > start {
				out = append(out, string(runes[start:i]))
			}
			start = i
		}
	}
	if start < len(runes) && start > 0 {
		out = append(out, string(runes[start:]))
	}
	return out
}

// bm25Stopwords is a conservative English + code-comment stopword list.
// Code-specific terms like "func", "var", "import" are NOT stopped because
// those can be the whole point of a query in a code-search context.
var bm25Stopwords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "is": {}, "are": {}, "was": {}, "were": {},
	"be": {}, "been": {}, "being": {}, "of": {}, "to": {}, "in": {}, "on": {},
	"at": {}, "by": {}, "for": {}, "with": {}, "from": {}, "as": {},
	"it": {}, "this": {}, "that": {}, "these": {}, "those": {},
	"and": {}, "or": {}, "but": {}, "if": {}, "then": {}, "else": {},
}
