package memory

const (
	conflictThreshold = 0.85
)

// ConflictResolver detects and resolves conflicting memories using embedding similarity.
type ConflictResolver struct {
	cold *PGCold
}

// NewConflictResolver creates a conflict resolver backed by PG cold store.
func NewConflictResolver(cold *PGCold) *ConflictResolver {
	return &ConflictResolver{cold: cold}
}

// FindConflicts returns existing memories whose embeddings are highly similar
// to the new memory (cosine similarity > threshold) and share the same type.
func (r *ConflictResolver) FindConflicts(newMemory *Memory, candidates []Memory) []Memory {
	if len(newMemory.Embedding) == 0 {
		return nil
	}

	var conflicts []Memory
	for _, c := range candidates {
		if len(c.Embedding) == 0 || c.Type != newMemory.Type {
			continue
		}
		sim := CosineSimilarity(newMemory.Embedding, c.Embedding)
		if sim >= conflictThreshold {
			conflicts = append(conflicts, c)
		}
	}
	return conflicts
}

// Resolve updates the conflicting old memory with new content and embedding.
// Returns the resolved memory (old ID, new content).
func (r *ConflictResolver) Resolve(old, new *Memory) *Memory {
	old.Content = new.Content
	old.Embedding = new.Embedding
	old.Score = new.Score
	return old
}
