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

	if len(conflicts) != 1 {
		t.Errorf("expected 1 conflict, got %d", len(conflicts))
	}
	if len(conflicts) > 0 && conflicts[0].Content != "User prefers spaces" {
		t.Errorf("wrong conflict detected: %s", conflicts[0].Content)
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

func TestConflictResolver_Resolve(t *testing.T) {
	resolver := NewConflictResolver(nil)

	old := &Memory{
		ID:             "old-id",
		UserID:         "user1",
		ProjectID:      "proj1",
		Type:           MemoryPreference,
		Content:        "Old content",
		Embedding:      []float32{0.5, 0.5, 0.0},
		Score:          0.8,
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
		Score:     0.9,
	}

	resolved := resolver.Resolve(old, new)

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
}
