package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/llm"
	"go.uber.org/zap"
)

// mockLLM returns pre-configured responses.
type mockLLM struct {
	response string
	err      error
}

func (m *mockLLM) ChatCompletion(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &llm.ChatResponse{Content: m.response}, nil
}

// mockStore tracks stored memories.
//
// `existing` is the pool returned by Retrieve; it's also the universe
// from which RetrieveCandidates draws its results so a single fixture
// covers both the legacy path and the P1 #9 dedup path.
//
// `retrieveCandidatesCallLimit` (set automatically) records the last
// limit RetrieveCandidates was called with — tests use this to assert
// "Extractor really did request K=30, not 5".
//
// `retrieveCalls` / `retrieveCandidatesCalls` count tracker. A test
// asserting "no legacy Retrieve was made when the embedder is wired"
// reads `retrieveCalls == 0` to make sure the new code path is taken.
type mockStore struct {
	stored                      []*Memory
	existing                    []Memory
	retrieveCalls               int
	retrieveCandidatesCalls     int
	retrieveCandidatesCallLimit int
	retrieveErr                 error
	retrieveCandidatesErr       error
}

func (m *mockStore) Store(_ context.Context, mem *Memory) error {
	m.stored = append(m.stored, mem)
	return nil
}

func (m *mockStore) Retrieve(_ context.Context, _, _, _ string, _ int) ([]Memory, error) {
	m.retrieveCalls++
	if m.retrieveErr != nil {
		return nil, m.retrieveErr
	}
	return m.existing, nil
}

// RetrieveCandidates honours `limit` so high-K tests can assert that
// rank-12 candidates are actually returned when limit ≥ 12 but skipped
// when limit < 12 — that's the whole point of P1 #9.
func (m *mockStore) RetrieveCandidates(_ context.Context, _, _ string, _ []float32, limit int) ([]Memory, error) {
	m.retrieveCandidatesCalls++
	m.retrieveCandidatesCallLimit = limit
	if m.retrieveCandidatesErr != nil {
		return nil, m.retrieveCandidatesErr
	}
	if limit <= 0 || limit >= len(m.existing) {
		return m.existing, nil
	}
	return m.existing[:limit], nil
}

// fakeEmbedder is a deterministic Embedder. It returns the first vec on
// every Embed call regardless of input — tests inject the "new content
// vector" they want isDuplicate to compare against the fixture.
type fakeEmbedder struct {
	vec []float32
	err error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}

func TestExtractor_LLMExtraction(t *testing.T) {
	llmResp := `[
		{"type": "preference", "content": "User prefers using Go for backend services", "importance": 0.8},
		{"type": "decision", "content": "Decided to use PostgreSQL for persistence", "importance": 0.9}
	]`

	ml := &mockLLM{response: llmResp}
	ms := &mockStore{}
	ext := NewExtractor(ms, ml, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1",
		"I prefer Go for backend", "Let's use PostgreSQL")

	if len(ms.stored) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(ms.stored))
	}

	if ms.stored[0].Type != MemoryPreference {
		t.Errorf("expected preference, got %s", ms.stored[0].Type)
	}
	if ms.stored[0].Score != 0.8 {
		t.Errorf("expected score 0.8, got %f", ms.stored[0].Score)
	}

	if ms.stored[1].Type != MemoryDecision {
		t.Errorf("expected decision, got %s", ms.stored[1].Type)
	}
	if ms.stored[1].Score != 0.9 {
		t.Errorf("expected score 0.9, got %f", ms.stored[1].Score)
	}
}

func TestExtractor_LLMExtraction_WithCodeFence(t *testing.T) {
	llmResp := "```json\n" + `[{"type": "knowledge", "content": "API uses REST", "importance": 0.7}]` + "\n```"

	ml := &mockLLM{response: llmResp}
	ms := &mockStore{}
	ext := NewExtractor(ms, ml, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1", "test", "test")

	if len(ms.stored) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(ms.stored))
	}
	if ms.stored[0].Content != "API uses REST" {
		t.Errorf("unexpected content: %s", ms.stored[0].Content)
	}
}

func TestExtractor_LLMExtraction_FiltersLowImportance(t *testing.T) {
	llmResp := `[
		{"type": "knowledge", "content": "Important fact", "importance": 0.8},
		{"type": "knowledge", "content": "Trivial detail", "importance": 0.2}
	]`

	ml := &mockLLM{response: llmResp}
	ms := &mockStore{}
	ext := NewExtractor(ms, ml, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1", "test", "test")

	if len(ms.stored) != 1 {
		t.Fatalf("expected 1 memory (filtered low importance), got %d", len(ms.stored))
	}
	if ms.stored[0].Content != "Important fact" {
		t.Errorf("wrong memory stored: %s", ms.stored[0].Content)
	}
}

func TestExtractor_Deduplication(t *testing.T) {
	llmResp := `[{"type": "preference", "content": "User prefers using tabs for indentation", "importance": 0.8}]`

	ml := &mockLLM{response: llmResp}
	ms := &mockStore{
		existing: []Memory{
			{Content: "User prefers tabs for indentation in code"},
		},
	}
	ext := NewExtractor(ms, ml, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1", "I prefer tabs", "ok")

	if len(ms.stored) != 0 {
		t.Fatalf("expected 0 memories (duplicate), got %d", len(ms.stored))
	}
}

func TestExtractor_HeuristicFallback(t *testing.T) {
	ms := &mockStore{}
	ext := NewExtractor(ms, nil, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1",
		"From now on, please use snake_case for variables", "Got it")

	if len(ms.stored) != 1 {
		t.Fatalf("expected 1 memory from heuristic, got %d", len(ms.stored))
	}
	if ms.stored[0].Type != MemoryPreference {
		t.Errorf("expected preference, got %s", ms.stored[0].Type)
	}
	if ms.stored[0].Score < 0.8 {
		t.Errorf("expected high importance for 'from now on', got %f", ms.stored[0].Score)
	}
}

func TestExtractor_HeuristicDecision(t *testing.T) {
	ms := &mockStore{}
	ext := NewExtractor(ms, nil, zap.NewNop())

	ext.ExtractFromInteraction(context.Background(), "user1", "proj1",
		"What should we use?", "I've decided to use Redis for caching")

	if len(ms.stored) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(ms.stored))
	}
	if ms.stored[0].Type != MemoryDecision {
		t.Errorf("expected decision, got %s", ms.stored[0].Type)
	}
}

func TestTextSimilarity(t *testing.T) {
	tests := []struct {
		a, b     string
		minScore float64
	}{
		{"user prefers tabs", "user prefers tabs", 0.99},
		{"user prefers tabs for indentation", "user prefers tabs in code", 0.5},
		{"completely different text", "nothing in common here", 0.0},
	}

	for _, tt := range tests {
		score := textSimilarity(tt.a, tt.b)
		if score < tt.minScore {
			t.Errorf("textSimilarity(%q, %q) = %f, want >= %f", tt.a, tt.b, score, tt.minScore)
		}
	}
}

func TestParseLLMResponse_InvalidJSON(t *testing.T) {
	result := parseLLMResponse("not json at all")
	if result != nil {
		t.Errorf("expected nil for invalid JSON, got %v", result)
	}
}

func TestParseLLMResponse_ClampsImportance(t *testing.T) {
	llmResp := `[
		{"type": "knowledge", "content": "test", "importance": -0.5},
		{"type": "knowledge", "content": "test2", "importance": 1.5}
	]`

	result := parseLLMResponse(llmResp)
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if result[0].Importance != 0 {
		t.Errorf("expected clamped to 0, got %f", result[0].Importance)
	}
	if result[1].Importance != 1 {
		t.Errorf("expected clamped to 1, got %f", result[1].Importance)
	}
}

func TestExtractSentence(t *testing.T) {
	text := "This is the first sentence. I prefer using Go for backend services. This is another sentence."
	result := extractSentence(text, "i prefer")

	if result == "" {
		t.Fatal("expected non-empty result")
	}
	if !contains(result, "prefer") {
		t.Errorf("result should contain 'prefer': %s", result)
	}
	// Should extract just the sentence, not the whole text
	if len(result) > 100 {
		t.Errorf("sentence too long: %d chars", len(result))
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexSubstr(s, substr) >= 0)
}

func indexSubstr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ----------------------------------------------------------------------
// P1 #9 — dedup candidate K
// ----------------------------------------------------------------------

// makeUnitVec returns a deterministic unit vector that differs from
// `base` by tiny rotations on `dim`. Used to seed the mockStore's
// `existing` so that exactly one entry matches the "new content" vector
// at cosine ≈ 1.0 and all others are near-orthogonal.
//
// Index 0 holds the matching vector; index 1..N hold near-zero
// similarity vectors. Tests then place the matching memory at a chosen
// rank inside `existing` (e.g. rank-12) by reordering the slice.
func makeDedupFixture(matchingIndex, total int) ([]Memory, []float32) {
	matchVec := []float32{1, 0, 0, 0}
	orthogonal := []float32{0, 1, 0, 0}

	out := make([]Memory, total)
	for i := 0; i < total; i++ {
		if i == matchingIndex {
			out[i] = Memory{ID: "match", Content: "user prefers tabs", Embedding: matchVec}
		} else {
			out[i] = Memory{
				ID:        fmt.Sprintf("noise-%d", i),
				Content:   "unrelated thought",
				Embedding: orthogonal,
			}
		}
	}
	// Caller passes its own newVec; we return the matching vec so the
	// happy-path test can re-use the exact same slice in the embedder.
	return out, matchVec
}

// TestExtractor_Dedup_HighLimitCatchesRank12 is the P1 #9 headline
// regression target. The matching duplicate sits at rank-12 in the
// candidate set. With the legacy K=5 (Retrieve), it would be cut off
// and we'd persist a duplicate. With K=30 (the new RetrieveCandidates
// path), it falls inside the window and isDuplicate returns true.
func TestExtractor_Dedup_HighLimitCatchesRank12(t *testing.T) {
	existing, matchVec := makeDedupFixture(11, 20) // rank 12 == index 11

	llmResp := `[{"type": "preference", "content": "User prefers tabs", "importance": 0.8}]`
	ml := &mockLLM{response: llmResp}
	ms := &mockStore{existing: existing}
	ext := NewExtractor(ms, ml, zap.NewNop())
	ext.SetEmbedder(&fakeEmbedder{vec: matchVec})

	ext.ExtractFromInteraction(context.Background(), "u1", "p1", "i like tabs", "ok")

	if len(ms.stored) != 0 {
		t.Fatalf("rank-12 duplicate must be caught at default K=30; got %d stored", len(ms.stored))
	}
	if ms.retrieveCandidatesCalls == 0 {
		t.Fatal("expected isDuplicate to call RetrieveCandidates when embedder is wired")
	}
	if ms.retrieveCandidatesCallLimit < 12 {
		t.Errorf("RetrieveCandidates limit %d would miss rank-12 dupes", ms.retrieveCandidatesCallLimit)
	}
	if ms.retrieveCalls != 0 {
		t.Errorf("legacy Retrieve must NOT fire when RetrieveCandidates returned data; got %d calls",
			ms.retrieveCalls)
	}
}

// TestExtractor_Dedup_LowLimitMissesRank12 documents the failure mode
// the P1 #9 fix prevents. Forcing K=5 reproduces the pre-fix behavior —
// rank-12 falls outside the window, dup is not detected, and a
// duplicate is persisted. This is the regression we shipped against.
func TestExtractor_Dedup_LowLimitMissesRank12(t *testing.T) {
	existing, matchVec := makeDedupFixture(11, 20)

	llmResp := `[{"type": "preference", "content": "User prefers tabs", "importance": 0.8}]`
	ms := &mockStore{existing: existing}
	ext := NewExtractor(ms, &mockLLM{response: llmResp}, zap.NewNop())
	ext.SetEmbedder(&fakeEmbedder{vec: matchVec})
	ext.SetDedupCandidateLimit(5) // re-create pre-P1#9 behavior

	ext.ExtractFromInteraction(context.Background(), "u1", "p1", "i like tabs", "ok")

	if len(ms.stored) != 1 {
		t.Fatalf("low K=5 should miss rank-12 dupes and store the new memory; got %d", len(ms.stored))
	}
}

// TestExtractor_Dedup_FallsBackToLegacyWhenNoEmbedder confirms the
// degraded path is preserved: no embedder → legacy Retrieve(content, 5)
// + n-gram Jaccard. Critical because some deployments run without an
// embedding endpoint, and removing the fallback would silently disable
// pre-Store dedup for them.
func TestExtractor_Dedup_FallsBackToLegacyWhenNoEmbedder(t *testing.T) {
	llmResp := `[{"type": "preference", "content": "User prefers using tabs for indentation", "importance": 0.8}]`
	ms := &mockStore{existing: []Memory{
		{Content: "User prefers tabs for indentation in code"},
	}}
	ext := NewExtractor(ms, &mockLLM{response: llmResp}, zap.NewNop())
	// Deliberately NOT calling SetEmbedder — fallback path under test.

	ext.ExtractFromInteraction(context.Background(), "u1", "p1", "tabs", "ok")

	if len(ms.stored) != 0 {
		t.Fatalf("n-gram fallback must catch the lexical near-duplicate; got %d stored", len(ms.stored))
	}
	if ms.retrieveCalls == 0 {
		t.Error("legacy Retrieve must fire when no embedder is wired")
	}
	if ms.retrieveCandidatesCalls != 0 {
		t.Errorf("RetrieveCandidates must NOT fire without an embedder; got %d calls",
			ms.retrieveCandidatesCalls)
	}
}

// TestExtractor_Dedup_FallsBackWhenEmbedderErrors verifies a fragile
// embedder doesn't poison dedup. If Embed returns an error, we fall
// through to the legacy path so dedup still runs (just at lower
// fidelity). The previous code did the same; we lock it in to prevent
// future refactors from silently disabling fallback.
func TestExtractor_Dedup_FallsBackWhenEmbedderErrors(t *testing.T) {
	llmResp := `[{"type": "preference", "content": "User prefers using tabs for indentation", "importance": 0.8}]`
	ms := &mockStore{existing: []Memory{
		{Content: "User prefers tabs for indentation in code"},
	}}
	ext := NewExtractor(ms, &mockLLM{response: llmResp}, zap.NewNop())
	ext.SetEmbedder(&fakeEmbedder{err: errEmbedderDown})

	ext.ExtractFromInteraction(context.Background(), "u1", "p1", "tabs", "ok")

	if ms.retrieveCalls == 0 {
		t.Error("legacy Retrieve must fire when embedder errors")
	}
	if len(ms.stored) != 0 {
		t.Fatalf("dedup should still catch lexical duplicate; got %d stored", len(ms.stored))
	}
}

// TestExtractor_SetDedupCandidateLimit_Clamped verifies the operator-
// facing knob refuses degenerate values. The defaults (5 floor, 200
// ceiling) are documented in the constants — a config typo writing
// dedup_candidate_limit = 1 would silently disable dedup without this
// guard.
func TestExtractor_SetDedupCandidateLimit_Clamped(t *testing.T) {
	tests := []struct {
		input, want int
	}{
		{0, minDedupCandidateLimit},   // 0 → 5
		{-1, minDedupCandidateLimit},  // negative → 5
		{1, minDedupCandidateLimit},   // <5 → 5
		{minDedupCandidateLimit, minDedupCandidateLimit},
		{30, 30},
		{maxDedupCandidateLimit, maxDedupCandidateLimit},
		{999, maxDedupCandidateLimit}, // >200 → 200
		{1_000_000, maxDedupCandidateLimit},
	}

	for _, tt := range tests {
		ext := NewExtractor(&mockStore{}, nil, zap.NewNop())
		ext.SetDedupCandidateLimit(tt.input)
		if ext.dedupCandidateLimit != tt.want {
			t.Errorf("SetDedupCandidateLimit(%d) = %d, want %d",
				tt.input, ext.dedupCandidateLimit, tt.want)
		}
	}
}

// TestExtractor_NewExtractor_DefaultDedupCandidateLimit locks in the
// default so a future refactor that adds a builder field doesn't reset
// to 0 (effectively disabling dedup).
func TestExtractor_NewExtractor_DefaultDedupCandidateLimit(t *testing.T) {
	ext := NewExtractor(&mockStore{}, nil, zap.NewNop())
	if ext.dedupCandidateLimit != defaultDedupCandidateLimit {
		t.Errorf("default dedupCandidateLimit = %d, want %d",
			ext.dedupCandidateLimit, defaultDedupCandidateLimit)
	}
}

// TestExtractor_Dedup_NoCandidatesNoOp: an empty candidate set means
// "library is empty for this tenant" — must short-circuit to false,
// never bump dedup_total, never claim duplicate.
func TestExtractor_Dedup_NoCandidatesNoOp(t *testing.T) {
	llmResp := `[{"type": "knowledge", "content": "new fact", "importance": 0.8}]`
	ms := &mockStore{existing: nil} // empty library
	ext := NewExtractor(ms, &mockLLM{response: llmResp}, zap.NewNop())
	ext.SetEmbedder(&fakeEmbedder{vec: []float32{1, 0, 0, 0}})

	ext.ExtractFromInteraction(context.Background(), "u1", "p1", "anything", "ok")

	if len(ms.stored) != 1 {
		t.Fatalf("empty library: new memory must be stored; got %d", len(ms.stored))
	}
}

var errEmbedderDown = errFake{"embedder unavailable"}

type errFake struct{ msg string }

func (e errFake) Error() string { return e.msg }

type mockCorePromoter struct {
	called bool
	content string
}

func (m *mockCorePromoter) AppendToSectionScoped(ctx context.Context, userID, projectID string, scope CoreMemoryScope, section, content string) error {
	m.called = true
	m.content = content
	return nil
}

func TestExtractor_AutoPromotesCoreMemory(t *testing.T) {
	llmResp := `[{"type":"preference","content":"I prefer tabs","importance":0.9}]`
	ms := &mockStore{}
	ml := &mockLLM{response: llmResp}
	promoter := &mockCorePromoter{}

	ext := NewExtractor(ms, ml, zap.NewNop()).WithCorePromoter(promoter)

	ext.ExtractFromInteraction(context.Background(), "u1", "p1", "I prefer tabs", "OK, tabs it is")

	// Wait for the async goroutine in the extractor to finish
	time.Sleep(50 * time.Millisecond)

	if !promoter.called {
		t.Error("expected core memory promoter to be called for importance 9 preference")
	}
	if promoter.content != "I prefer tabs" {
		t.Errorf("expected promoted content 'I prefer tabs', got %q", promoter.content)
	}
}
