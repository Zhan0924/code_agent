package memory

import "time"

// MemoryType categorizes what kind of information a memory holds.
type MemoryType string

const (
	MemoryPreference MemoryType = "preference"
	MemoryDecision   MemoryType = "decision"
	MemoryKnowledge  MemoryType = "knowledge"
	MemoryPattern    MemoryType = "pattern"
)

// Memory represents a single unit of long-term memory.
type Memory struct {
	ID             string     `json:"id" db:"id"`
	UserID         string     `json:"user_id" db:"user_id"`
	ProjectID      string     `json:"project_id" db:"project_id"`
	Type           MemoryType `json:"type" db:"type"`
	Content        string     `json:"content" db:"content"`
	Embedding      []float32  `json:"embedding,omitempty" db:"embedding"`
	Score          float64    `json:"score" db:"score"`
	AccessCount    int        `json:"access_count" db:"access_count"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	LastAccessedAt time.Time  `json:"last_accessed_at" db:"last_accessed_at"`
}

// MemoryStore abstracts the persistence layer for memories.
type MemoryStore interface {
	Store(m *Memory) error
	Retrieve(userID, projectID string, query string, limit int) ([]Memory, error)
	RetrieveByVector(embedding []float32, userID, projectID string, limit int) ([]Memory, error)
	Update(m *Memory) error
	Touch(id string) error
	Decay(olderThan time.Duration, factor float64) (int, error)
}

// RetrievalResult pairs a memory with its relevance score from search.
type RetrievalResult struct {
	Memory    Memory  `json:"memory"`
	Relevance float64 `json:"relevance"`
}
