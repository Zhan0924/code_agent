package memory

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConflictResolver_FindConflicts(t *testing.T) {
	resolver := NewConflictResolver(nil)

	newMem := &Memory{
		ID:        uuid.New().String(),
		Type:      MemoryPreference,
		Content:   "User prefers spaces over tabs",
		Embedding: []float32{1.0, 0.0, 0.0},
	}

	candidates := []Memory{
		{
			ID:        uuid.New().String(),
			Type:      MemoryPreference,
			Content:   "User prefers spaces",
			Embedding: []float32{0.99, 0.01, 0.0}, // Very similar
		},
		{
			ID:        uuid.New().String(),
			Type:      MemoryPreference,
			Content:   "User likes dark mode",
			Embedding: []float32{0.0, 1.0, 0.0}, // Orthogonal
		},
		{
			ID:        uuid.New().String(),
			Type:      MemoryDecision,
			Content:   "Use React for frontend",
			Embedding: []float32{0.99, 0.01, 0.0}, // Similar but different type
		},
	}

	conflicts := resolver.FindConflicts(newMem, candidates)

	if len(conflicts) != 2 {
		t.Errorf("expected 2 conflicts, got %d", len(conflicts))
	}
	
	// Should detect both the same-type and cross-type conflicts
	hasSpaces := false
	hasReact := false
	for _, c := range conflicts {
		if c.Content == "User prefers spaces" {
			hasSpaces = true
		} else if c.Content == "Use React for frontend" {
			hasReact = true
		}
	}
	
	if !hasSpaces || !hasReact {
		t.Errorf("wrong conflicts detected: %+v", conflicts)
	}
}

func TestConflictResolver_NoConflictWhenNoEmbedding(t *testing.T) {
	resolver := NewConflictResolver(nil)

	newMem := &Memory{
		ID:      uuid.New().String(),
		Type:    MemoryPreference,
		Content: "User prefers spaces",
	}

	candidates := []Memory{
		{
			ID:        uuid.New().String(),
			Type:      MemoryPreference,
			Content:   "User prefers tabs",
			Embedding: []float32{1.0, 0.0, 0.0},
		},
	}

	conflicts := resolver.FindConflicts(newMem, candidates)
	if len(conflicts) != 0 {
		t.Errorf("expected no conflicts when new memory has no embedding, got %d", len(conflicts))
	}
}

// TestConflictResolver_Resolve_Override exercises the path where the new
// memory's importance score clearly exceeds the old's (gap > MarginToOverride
// = 0.2 default). Expected: content + embedding + score are all taken from
// the new memory, while ID + CreatedAt stay with the old one.
func TestConflictResolver_Resolve_Override(t *testing.T) {
	resolver := NewConflictResolver(nil)

	old := &Memory{
		ID:             "old-id",
		UserID:         "user1",
		ProjectID:      "proj1",
		Type:           MemoryPreference,
		Content:        "Old content",
		Embedding:      []float32{0.5, 0.5, 0.0},
		Score:          0.4,
		CreatedAt:      time.Now().Add(-24 * time.Hour),
		LastAccessedAt: time.Now().Add(-1 * time.Hour),
	}

	new := &Memory{
		ID:        "new-id",
		UserID:    "user1",
		ProjectID: "proj1",
		Type:      MemoryPreference,
		Content:   "New content",
		Embedding: []float32{0.6, 0.4, 0.0},
		Score:     0.9, // gap = 0.5, well above margin 0.2 → override
	}

	resolved, outcome := resolver.ResolveWithOutcome(old, new)
	if outcome != OutcomeOverride {
		t.Fatalf("expected outcome=override, got %s", outcome)
	}
	if resolved.ID != "old-id" {
		t.Errorf("expected old ID to be preserved, got %s", resolved.ID)
	}
	if resolved.Content != "New content" {
		t.Errorf("expected new content, got %s", resolved.Content)
	}
	if resolved.Score != 0.9 {
		t.Errorf("expected new score, got %f", resolved.Score)
	}
	if len(resolved.Embedding) != 3 || resolved.Embedding[0] != 0.6 {
		t.Errorf("expected new embedding, got %v", resolved.Embedding)
	}
	if resolved.AccessCount != 1 {
		t.Errorf("expected AccessCount bumped to 1, got %d", resolved.AccessCount)
	}
}

// TestConflictResolver_Resolve_Preserve covers the "don't let a noisy
// low-importance restatement clobber a curated high-score memory" path.
func TestConflictResolver_Resolve_Preserve(t *testing.T) {
	resolver := NewConflictResolver(nil)

	old := &Memory{
		ID:        "old-id",
		Type:      MemoryPreference,
		Content:   "Old high-quality content",
		Embedding: []float32{0.5, 0.5, 0.0},
		Score:     0.95,
	}
	new := &Memory{
		Type:      MemoryPreference,
		Content:   "Low-importance restatement",
		Embedding: []float32{0.6, 0.4, 0.0},
		Score:     0.4, // gap = -0.55, well below -margin 0.2 → preserve
	}

	resolved, outcome := resolver.ResolveWithOutcome(old, new)
	if outcome != OutcomePreserve {
		t.Fatalf("expected outcome=preserve, got %s", outcome)
	}
	if resolved.Content != "Old high-quality content" {
		t.Errorf("expected old content to be preserved, got %s", resolved.Content)
	}
}

// TestConflictResolver_Resolve_Merge covers the comparable-importance path
// — embedding gets refreshed (LLM may have a sharper phrasing) but content
// is kept to avoid jitter, and score is blended with recency weight.
func TestConflictResolver_Resolve_Merge(t *testing.T) {
	resolver := NewConflictResolver(nil)

	old := &Memory{
		ID:             "old-id",
		Type:           MemoryPreference,
		Content:        "Old content",
		Embedding:      []float32{0.5, 0.5, 0.0},
		Score:          0.7,
		LastAccessedAt: time.Now(),
	}
	new := &Memory{
		Type:      MemoryPreference,
		Content:   "Reworded content",
		Embedding: []float32{0.6, 0.4, 0.0},
		Score:     0.75, // gap = 0.05, within margin → merge
	}

	resolved, outcome := resolver.ResolveWithOutcome(old, new)
	if outcome != OutcomeMerge {
		t.Fatalf("expected outcome=merge, got %s", outcome)
	}
	if resolved.Content != "Old content" {
		t.Errorf("merge should keep old content, got %s", resolved.Content)
	}
	if len(resolved.Embedding) != 3 || resolved.Embedding[0] != 0.6 {
		t.Errorf("merge should refresh embedding, got %v", resolved.Embedding)
	}
}

// TestConflictResolver_PickAnchor_ByScore is the headline P1 #7
// assertion: among N conflicting memories the highest-score one wins
// (operators / LLMs use score to mark importance — keep that signal).
func TestConflictResolver_PickAnchor_ByScore(t *testing.T) {
	conflicts := []Memory{
		{ID: "low", Score: 0.3},
		{ID: "high", Score: 0.9},
		{ID: "mid", Score: 0.6},
	}
	idx := PickAnchor(conflicts)
	if conflicts[idx].ID != "high" {
		t.Fatalf("expected anchor=high, got %s", conflicts[idx].ID)
	}
}

// TestConflictResolver_PickAnchor_TieBreaksByAccessCount: when scores
// match, the more-referenced entry wins. This protects "popular" copies
// of a memory even if they're not the highest-scored one.
func TestConflictResolver_PickAnchor_TieBreaksByAccessCount(t *testing.T) {
	conflicts := []Memory{
		{ID: "rare", Score: 0.5, AccessCount: 1},
		{ID: "popular", Score: 0.5, AccessCount: 42},
		{ID: "moderate", Score: 0.5, AccessCount: 10},
	}
	idx := PickAnchor(conflicts)
	if conflicts[idx].ID != "popular" {
		t.Fatalf("expected anchor=popular, got %s", conflicts[idx].ID)
	}
}

// TestConflictResolver_PickAnchor_TieBreaksByCreatedAt: equal score
// and access count → prefer the older entry. Downstream code may hold
// the older ID; preserving it minimises churn.
func TestConflictResolver_PickAnchor_TieBreaksByCreatedAt(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := old.Add(time.Hour)
	new := mid.Add(time.Hour)
	conflicts := []Memory{
		{ID: "mid", Score: 0.5, AccessCount: 5, CreatedAt: mid},
		{ID: "new", Score: 0.5, AccessCount: 5, CreatedAt: new},
		{ID: "old", Score: 0.5, AccessCount: 5, CreatedAt: old},
	}
	idx := PickAnchor(conflicts)
	if conflicts[idx].ID != "old" {
		t.Fatalf("expected anchor=old, got %s", conflicts[idx].ID)
	}
}

// TestConflictResolver_PickAnchor_FinalTieByID: with score, access
// count, and CreatedAt all equal, fall back to ID lexicographic order
// so snapshot tests are deterministic.
func TestConflictResolver_PickAnchor_FinalTieByID(t *testing.T) {
	now := time.Now()
	conflicts := []Memory{
		{ID: "b", Score: 0.5, AccessCount: 1, CreatedAt: now},
		{ID: "a", Score: 0.5, AccessCount: 1, CreatedAt: now},
		{ID: "c", Score: 0.5, AccessCount: 1, CreatedAt: now},
	}
	idx := PickAnchor(conflicts)
	if conflicts[idx].ID != "a" {
		t.Fatalf("expected anchor=a (lex first), got %s", conflicts[idx].ID)
	}
}

// TestConflictResolver_PickAnchor_SingleConflict: degenerate case —
// one conflict means it IS the anchor. Otherwise we'd panic or
// silently misbehave on the N==1 dedup path.
func TestConflictResolver_PickAnchor_SingleConflict(t *testing.T) {
	conflicts := []Memory{{ID: "only", Score: 0.5}}
	idx := PickAnchor(conflicts)
	if idx != 0 {
		t.Fatalf("single conflict → idx must be 0, got %d", idx)
	}
}

// TestConflictResolver_ReinforceFromDup_AccessCountAccumulates: a
// duplicate's AccessCount + 1 ("encountered as dup") must be added to
// the anchor, so the surviving entry inherits cumulative reinforcement
// across all redundant copies (the Hebbian intent).
func TestConflictResolver_ReinforceFromDup_AccessCountAccumulates(t *testing.T) {
	resolver := NewConflictResolver(nil)
	anchor := &Memory{ID: "anchor", AccessCount: 3, LastAccessedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	dup := Memory{ID: "dup", AccessCount: 7, LastAccessedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}

	resolver.ReinforceFromDup(anchor, dup)

	if anchor.AccessCount != 11 { // 3 + 1 + 7
		t.Fatalf("expected AccessCount=11 (3 anchor + 1 encounter + 7 dup), got %d", anchor.AccessCount)
	}
	if !anchor.LastAccessedAt.Equal(dup.LastAccessedAt) {
		t.Fatalf("LastAccessedAt should advance to dup's later timestamp; got %v", anchor.LastAccessedAt)
	}
}

// TestConflictResolver_ReinforceFromDup_KeepsLaterAnchorTimestamp:
// anchor's LastAccessedAt should be preserved if it's already after
// the dup's — never regress.
func TestConflictResolver_ReinforceFromDup_KeepsLaterAnchorTimestamp(t *testing.T) {
	resolver := NewConflictResolver(nil)
	anchorTime := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	anchor := &Memory{ID: "anchor", AccessCount: 5, LastAccessedAt: anchorTime}
	dup := Memory{ID: "dup", AccessCount: 2, LastAccessedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	resolver.ReinforceFromDup(anchor, dup)

	if !anchor.LastAccessedAt.Equal(anchorTime) {
		t.Fatalf("anchor LastAccessedAt must not regress; got %v", anchor.LastAccessedAt)
	}
}

// TestConflictResolver_MaxConflicts_DefaultsTo32: the dedup cap must
// default to 32 (defaultMaxConflictsToDedup) so a misconfigured
// install doesn't accidentally process unbounded candidate sets.
func TestConflictResolver_MaxConflicts_DefaultsTo32(t *testing.T) {
	r := NewConflictResolver(nil)
	if got := r.MaxConflicts(); got != 32 {
		t.Fatalf("expected default MaxConflicts=32, got %d", got)
	}

	rCustom := NewConflictResolverWithConfig(nil, ConflictResolverConfig{
		MaxConflictsToDedup: 5,
	})
	if got := rCustom.MaxConflicts(); got != 5 {
		t.Fatalf("expected MaxConflicts=5, got %d", got)
	}

	// Explicit zero → falls back to default.
	rZero := NewConflictResolverWithConfig(nil, ConflictResolverConfig{
		MaxConflictsToDedup: 0,
	})
	if got := rZero.MaxConflicts(); got != 32 {
		t.Fatalf("expected zero to default to 32, got %d", got)
	}
}

// TestConflictResolver_PickAnchor_EmptyPanics: contract violation
// must surface loudly. Production callers always check len(conflicts)
// > 0 before invoking; nobody should rely on a sentinel return.
func TestConflictResolver_PickAnchor_EmptyPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty conflicts")
		}
	}()
	PickAnchor(nil)
}

// TestConflictResolver_DedupKernel_AnchorAbsorbsAllReinforcement is a
// pure-logic exercise of the P1 #7 dedup orchestration: given N
// conflicts (anchor + dups), the anchor's AccessCount must equal
// its original count + N-1 (one "encounter" per dup) + sum of dup
// AccessCounts. This is the contract HybridStore.dedupMerge relies on.
func TestConflictResolver_DedupKernel_AnchorAbsorbsAllReinforcement(t *testing.T) {
	resolver := NewConflictResolver(nil)

	conflicts := []Memory{
		{ID: "anchor", Score: 0.9, AccessCount: 5, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "dup-a", Score: 0.5, AccessCount: 3},
		{ID: "dup-b", Score: 0.5, AccessCount: 7},
		{ID: "dup-c", Score: 0.5, AccessCount: 1},
	}
	idx := PickAnchor(conflicts)
	if conflicts[idx].ID != "anchor" {
		t.Fatalf("anchor selection broken: got %s", conflicts[idx].ID)
	}

	anchor := conflicts[idx]
	startingAccess := anchor.AccessCount
	for i, c := range conflicts {
		if i == idx {
			continue
		}
		resolver.ReinforceFromDup(&anchor, c)
	}

	// Expected: 5 (anchor) + (1+3) + (1+7) + (1+1) = 5 + 4 + 8 + 2 = 19
	expected := startingAccess + 1 + 3 + 1 + 7 + 1 + 1
	if anchor.AccessCount != expected {
		t.Fatalf("expected anchor.AccessCount=%d after absorbing 3 dups, got %d",
			expected, anchor.AccessCount)
	}
}
