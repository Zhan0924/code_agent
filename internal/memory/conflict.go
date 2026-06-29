package memory

import (
	"math"
	"time"
)

// defaultConflictThreshold 是 cosine ≥ X 视作语义重复的下限。
// 0.85 是经验值，但对不同 embedding 模型最优值差异极大（OpenAI
// text-embedding-3 通常 0.7+ 已同义，bge-large 需要 0.85+）。
// 通过 NewConflictResolverWithConfig 可覆盖。
const defaultConflictThreshold = 0.85

// defaultMaxConflictsToDedup caps how many duplicates we dedupe in one
// Store call. Even a runaway candidate set (e.g. 200 conflicts) can't
// then take down the cold transaction or run a multi-second DELETE.
const defaultMaxConflictsToDedup = 32

// ConflictResolverConfig parameterises the resolver. All fields are
// optional — zero values fall back to documented defaults.
type ConflictResolverConfig struct {
	// Threshold: cosine similarity above which two same-type memories are
	// treated as "the same thing said again". Default 0.85.
	Threshold float64
	// RecencyHalfLife: how fast the "freshness" bonus of the new memory
	// decays vs the old. 0 disables recency weighting. Default: 7 days.
	RecencyHalfLife time.Duration
	// PreserveHighScore: if true, when the OLD memory's score is meaningfully
	// higher than the new one (Score - new.Score > MarginToOverride), keep
	// the old content and just refresh metadata. This protects high-quality
	// curated memories from being clobbered by low-importance restatements.
	PreserveHighScore bool
	// MarginToOverride: score gap required for the *new* memory to outright
	// replace the old content (rather than reinforce it). Default 0.2.
	MarginToOverride float64
	// MaxConflictsToDedup bounds how many duplicates HybridStore.Store
	// will collapse into a single anchor in one Store call. Operators
	// can lower this on installs where candidate over-fetching is noisy.
	// Default 32 (defaultMaxConflictsToDedup).
	MaxConflictsToDedup int
}

func (c ConflictResolverConfig) withDefaults() ConflictResolverConfig {
	if c.Threshold <= 0 {
		c.Threshold = defaultConflictThreshold
	}
	if c.RecencyHalfLife == 0 {
		c.RecencyHalfLife = 7 * 24 * time.Hour
	}
	if c.MarginToOverride <= 0 {
		c.MarginToOverride = 0.2
	}
	if c.MaxConflictsToDedup <= 0 {
		c.MaxConflictsToDedup = defaultMaxConflictsToDedup
	}
	return c
}

// MaxConflicts returns the configured cap on dedup batch size.
// Exposed so HybridStore can clamp its candidate set without poking
// at the private cfg field.
func (r *ConflictResolver) MaxConflicts() int {
	return r.cfg.MaxConflictsToDedup
}

// ResolveOutcome describes what Resolve decided. Observable via metrics
// to detect "we always preserve" or "we always override" pathologies.
type ResolveOutcome string

const (
	OutcomeMerge    ResolveOutcome = "merge"    // comparable importance → blend
	OutcomeOverride ResolveOutcome = "override" // new clearly supersedes old
	OutcomePreserve ResolveOutcome = "preserve" // old high-score wins
)

// ConflictResolver detects and resolves conflicting memories using embedding similarity.
type ConflictResolver struct {
	cold *PGCold
	cfg  ConflictResolverConfig
}

// NewConflictResolver creates a conflict resolver with default config.
func NewConflictResolver(cold *PGCold) *ConflictResolver {
	return NewConflictResolverWithConfig(cold, ConflictResolverConfig{
		PreserveHighScore: true,
	})
}

// NewConflictResolverWithConfig allows overriding the threshold + policies.
func NewConflictResolverWithConfig(cold *PGCold, cfg ConflictResolverConfig) *ConflictResolver {
	return &ConflictResolver{cold: cold, cfg: cfg.withDefaults()}
}

// FindConflicts returns existing memories whose embeddings are highly similar
// to the new memory (cosine similarity ≥ threshold) and share the same type.
//
// Same-type constraint is important: a "preference" should never silently
// overwrite a "knowledge" entry, even if their embeddings drift close.
func (r *ConflictResolver) FindConflicts(newMemory *Memory, candidates []Memory) []Memory {
	if len(newMemory.Embedding) == 0 {
		return nil
	}

	var conflicts []Memory
	for _, c := range candidates {
		// Ensure both have valid embeddings to compare.
		// Note: We intentionally allow cross-type deduplication (e.g. preference vs knowledge).
		if len(c.Embedding) == 0 {
			continue
		}
		sim := CosineSimilarity(newMemory.Embedding, c.Embedding)
		if sim >= r.cfg.Threshold {
			conflicts = append(conflicts, c)
		}
	}
	return conflicts
}

// Resolve is a thin wrapper around ResolveWithOutcome that discards the
// outcome — kept for callers that don't care (tests, simple paths). Most
// production code should use ResolveWithOutcome and report the outcome
// to metrics.
func (r *ConflictResolver) Resolve(old, new *Memory) *Memory {
	m, _ := r.ResolveWithOutcome(old, new)
	return m
}

// ResolveWithOutcome merges a new memory into an existing (conflicting) old
// memory using a score-aware policy. Replaces the legacy "always-overwrite"
// behaviour:
//
//  1. AccessCount += 1 — repeated expression is *reinforcement* (Hebbian).
//  2. Score := blend(old.Score, new.Score, recencyWeight) — never lose
//     a high-quality curated memory just because a noisy restatement came
//     in with Score=0.3.
//  3. Content/Embedding replacement is gated:
//     - new.Score > old.Score + Margin → replace (the user genuinely
//       updated their position, e.g. "actually switch to Vue").
//     - else → keep old content (treat new as reinforcement).
//  4. LastAccessedAt := now (touch).
//  5. Original ID + CreatedAt always preserved for auditability.
//
// Returns the resolved memory and the outcome; caller is responsible for
// persisting it (and reporting outcome to metrics if desired).
func (r *ConflictResolver) ResolveWithOutcome(old, new *Memory) (*Memory, ResolveOutcome) {
	now := time.Now()

	// Reinforcement signal — even when we keep the old content, the fact
	// that the user re-expressed it bumps access count + freshness.
	old.AccessCount++
	old.LastAccessedAt = now
	old.UpdatedAt = now

	// Score blending with recency weight: newer signal carries more weight
	// but old score still matters. Half-life lets us tune how aggressively
	// older curation dominates.
	w := r.recencyWeight(old.LastAccessedAt, now)
	blendedScore := (1-w)*old.Score + w*new.Score
	if blendedScore > 1.0 {
		blendedScore = 1.0
	}

	// Promote type if the new memory has a higher-priority type.
	if typePriority(new.Type) > typePriority(old.Type) {
		old.Type = new.Type
	}

	scoreGap := new.Score - old.Score
	switch {
	case r.cfg.PreserveHighScore && scoreGap < -r.cfg.MarginToOverride:
		// Old memory is meaningfully better-scored — keep content, just
		// bump freshness + score blend.
		old.Score = blendedScore
		return old, OutcomePreserve

	case scoreGap > r.cfg.MarginToOverride:
		// New memory genuinely supersedes old (e.g. user revised opinion).
		old.Content = new.Content
		old.Embedding = new.Embedding
		old.Score = new.Score // honor the new "this is more important now"
		return old, OutcomeOverride

	default:
		// Comparable importance: refresh embedding (LLM may have produced a
		// more precise phrasing) but preserve old content to avoid jitter,
		// and blend the score.
		old.Embedding = new.Embedding
		old.Score = blendedScore
		return old, OutcomeMerge
	}
}

// PickAnchor selects the "winner" among N conflicting memories to keep.
// The other N-1 will be deleted by HybridStore's dedup branch after
// their reinforcement signal is folded into the anchor (ReinforceFromDup).
//
// Selection is fully deterministic (no randomness, no time.Now) so the
// same input always picks the same anchor — critical for snapshot tests
// and for retry idempotency. Priority order:
//
//  1. Score DESC — highest-quality entry wins
//  2. AccessCount DESC — more-referenced one wins on tie
//  3. CreatedAt ASC — older entry wins on tie (downstream code may
//     hold its ID; younger duplicates were likely "echoes")
//  4. ID ASC (lexicographic) — final tie-break for determinism
//
// Returns the index into conflicts of the chosen anchor. Panics on
// empty input — that's a contract violation, not a runtime error to
// be tolerated.
func PickAnchor(conflicts []Memory) int {
	if len(conflicts) == 0 {
		panic("PickAnchor: empty conflicts")
	}
	best := 0
	for i := 1; i < len(conflicts); i++ {
		if anchorBeats(conflicts[i], conflicts[best]) {
			best = i
		}
	}
	return best
}

// anchorBeats reports whether candidate a should replace b as the
// running anchor. Returns true if a is strictly better on the first
// dimension that differs.
func anchorBeats(a, b Memory) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.AccessCount != b.AccessCount {
		return a.AccessCount > b.AccessCount
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// ReinforceFromDup folds a duplicate's "this memory was used"
// signal into the anchor without touching content/score/embedding —
// the anchor was chosen precisely *because* its content & score are
// the ones we want to keep. We only want to inherit the dup's
// Hebbian counters so the anchor reflects total reinforcement
// across all the redundant copies.
//
// Behavior:
//   - AccessCount += 1 + dup.AccessCount
//     (1 for "encountered as duplicate", plus all of dup's history)
//   - LastAccessedAt advances to max(anchor, dup)
//
// Note we deliberately do NOT call recencyWeight or blend scores —
// that's ResolveWithOutcome's job for the new memory. Dups are
// already-merged signal; treating them as "new" would double-blend.
func (r *ConflictResolver) ReinforceFromDup(anchor *Memory, dup Memory) {
	anchor.AccessCount += 1 + dup.AccessCount
	if dup.LastAccessedAt.After(anchor.LastAccessedAt) {
		anchor.LastAccessedAt = dup.LastAccessedAt
	}
	if typePriority(dup.Type) > typePriority(anchor.Type) {
		anchor.Type = dup.Type
	}
}

// typePriority assigns a numerical priority to memory types to ensure
// higher-value semantic abstractions survive cross-type deduplication.
func typePriority(t MemoryType) int {
	switch t {
	case MemoryPreference:
		return 5
	case MemoryKnowledge:
		return 4
	case MemoryPattern:
		return 3
	case MemoryTypeEpisodic:
		return 2
	case MemoryTypeSemantic:
		return 1
	default:
		return 0
	}
}

// recencyWeight returns a [0,1] weight giving more influence to the new
// signal when the old memory hasn't been touched recently. Formula is
// 1 - 2^(-age/halflife) so the very old (age >> halflife) approaches 1.
func (r *ConflictResolver) recencyWeight(oldTouched, now time.Time) float64 {
	if r.cfg.RecencyHalfLife <= 0 {
		return 0.5 // even blend when feature disabled
	}
	age := now.Sub(oldTouched)
	if age <= 0 {
		return 0.5
	}
	ratio := float64(age) / float64(r.cfg.RecencyHalfLife)
	w := 1 - math.Pow(2, -ratio)
	if w < 0 {
		return 0
	}
	if w > 0.95 {
		return 0.95
	}
	return w
}
