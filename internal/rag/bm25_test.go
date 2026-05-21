package rag

import (
	"strings"
	"testing"

	"github.com/agent/code_agent/internal/models"
)

// TestBM25_BasicRanking confirms that a query for a distinctive term
// ranks the doc that actually contains that term above an unrelated doc.
// This is the minimum correctness property — before the rewrite,
// SearchSparse returned a hardcoded score of 0.5 for every "matching"
// doc, so ranking was effectively random.
func TestBM25_BasicRanking(t *testing.T) {
	idx := NewBM25Index()
	idx.Build([]models.CodeChunk{
		{ID: "a", Content: "the quick brown fox jumps over the lazy dog", SymbolName: "fable"},
		{ID: "b", Content: "authentication middleware validates JWT tokens", SymbolName: "authMiddleware"},
		{ID: "c", Content: "red green blue yellow purple", SymbolName: "colors"},
	})

	got := idx.Search("JWT authentication", 3)
	if len(got) == 0 {
		t.Fatal("expected at least one hit for a term that appears in a doc")
	}
	if got[0].Chunk.ID != "b" {
		t.Errorf("expected doc 'b' to rank first, got %q; full ordering: %v",
			got[0].Chunk.ID, chunkIDsOf(got))
	}
}

// TestBM25_UnknownTermsReturnEmpty verifies the empty-result contract.
func TestBM25_UnknownTermsReturnEmpty(t *testing.T) {
	idx := NewBM25Index()
	idx.Build([]models.CodeChunk{
		{ID: "a", Content: "alpha beta", SymbolName: "x"},
	})
	got := idx.Search("nonexistent terms here", 5)
	if len(got) != 0 {
		t.Errorf("expected empty results for unmatched query, got %d", len(got))
	}
}

// TestBM25_EmptyIndexReturnsEmpty covers the zero-corpus guard.
func TestBM25_EmptyIndexReturnsEmpty(t *testing.T) {
	idx := NewBM25Index()
	// No Build call.
	got := idx.Search("anything", 5)
	if got == nil {
		t.Error("Search on empty index must return a non-nil empty slice (callers skip nil-check)")
	}
}

// TestBM25_CamelCaseTokenization verifies that querying "http client" hits
// identifiers like HTTPClient or httpClient, which the prior substring
// match on symbol_name could not do. Uses a 4-doc corpus because BM25's
// IDF formula drives terms toward zero when df/N ≈ 0.5 — a property that
// is mathematically correct but surprising on tiny corpora.
func TestBM25_CamelCaseTokenization(t *testing.T) {
	idx := NewBM25Index()
	idx.Build([]models.CodeChunk{
		{ID: "a", Content: "func NewHTTPClient() {}", SymbolName: "NewHTTPClient"},
		{ID: "b", Content: "type apiError struct{}", SymbolName: "apiError"},
		{ID: "c", Content: "func sortSlice(s []int) {}", SymbolName: "sortSlice"},
		{ID: "d", Content: "const maxRetries = 3", SymbolName: "maxRetries"},
	})
	got := idx.Search("http client", 4)
	if len(got) == 0 || got[0].Chunk.ID != "a" {
		t.Errorf("expected NewHTTPClient to rank first for 'http client'; got: %v", chunkIDsOf(got))
	}
}

// TestBM25_IDFDownweightsCommonTerms — a word appearing in every document
// carries no discriminative power. BM25IDF should return 0 for such terms,
// so rankings should depend on the discriminative term, not the common one.
func TestBM25_IDFDownweightsCommonTerms(t *testing.T) {
	idx := NewBM25Index()
	idx.Build([]models.CodeChunk{
		{ID: "a", Content: "the zephyr flies", SymbolName: ""},
		{ID: "b", Content: "the quokka hops", SymbolName: ""},
		{ID: "c", Content: "the kookaburra laughs", SymbolName: ""},
	})
	got := idx.Search("the quokka", 3)
	if len(got) == 0 {
		t.Fatal("expected a hit")
	}
	if got[0].Chunk.ID != "b" {
		t.Errorf("discriminative term 'quokka' should rank 'b' first; got %v", chunkIDsOf(got))
	}
}

// TestSplitCamel exercises the identifier splitter directly — edge cases
// in tokenisation here are what let queries like "http client" find
// HTTPClient. Regressions would silently degrade recall.
func TestSplitCamel(t *testing.T) {
	cases := map[string][]string{
		"HTTPClient":     {"HTTPClient", "HTTP", "Client"},
		"parseJSON":      {"parseJSON", "parse", "JSON"},
		"simple":         {"simple"},
		"ABCDef":         {"ABCDef", "ABC", "Def"},
		"":               nil,
		"already_split":  {"already_split"}, // underscore handled upstream; splitCamel sees only letters
		"XMLHttpRequest": {"XMLHttpRequest", "XML", "Http", "Request"},
	}
	for in, want := range cases {
		got := splitCamel(in)
		if !stringSlicesEqual(got, want) {
			t.Errorf("splitCamel(%q) = %v, want %v", in, got, want)
		}
	}
}

func chunkIDsOf(rs []models.RetrievalResult) []string {
	ids := make([]string, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, r.Chunk.ID)
	}
	return ids
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTokenize_DropsStopwords guards against regressions where the stopword
// filter silently stops affecting recall.
func TestTokenize_DropsStopwords(t *testing.T) {
	out := tokenizeForBM25("the quick brown FOX")
	joined := strings.Join(out, " ")
	if strings.Contains(joined, "the") {
		t.Errorf("expected stopwords dropped, got: %v", out)
	}
	if !strings.Contains(joined, "fox") {
		t.Errorf("expected lowercased tokens, got: %v", out)
	}
}
