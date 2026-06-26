package memory

import (
	"context"
	"time"
)

// MemoryType categorizes what kind of information a memory holds.
type MemoryType string

const (
	MemoryPreference   MemoryType = "preference"
	MemoryDecision     MemoryType = "decision"
	MemoryKnowledge    MemoryType = "knowledge"
	MemoryPattern      MemoryType = "pattern"
	MemoryTypeEpisodic MemoryType = "episodic"
	MemoryTypeSemantic MemoryType = "semantic"
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
	UpdatedAt      time.Time  `json:"updated_at" db:"updated_at"`
	LastAccessedAt time.Time  `json:"last_accessed_at" db:"last_accessed_at"`
	// DistilledAt marks an episodic memory that has been consumed by the
	// Distiller — non-nil means "already folded into a semantic memory",
	// so Distiller.ListByType skips it next run. Nil for non-episodic
	// types and for unconsumed episodes.
	DistilledAt *time.Time `json:"distilled_at,omitempty" db:"distilled_at"`
}

// MemoryStore abstracts the persistence layer for memories.
//
// TouchBatch is the read-path "I just retrieved these N memories"
// signal — folding many touches into one round-trip is what lets
// Decay use last_accessed_at honestly without doubling read DB QPS.
// See PGCold.TouchBatch for the SQL and docs/architecture/25_memory.md
// §19 for the rationale.
type MemoryStore interface {
	Store(m *Memory) error
	Retrieve(userID, projectID string, query string, limit int) ([]Memory, error)
	RetrieveByVector(embedding []float32, userID, projectID string, limit int) ([]Memory, error)
	Update(m *Memory) error
	Touch(id string) error
	TouchBatch(ctx context.Context, ids []string) error
	Decay(ctx context.Context, olderThan time.Duration, factor float64) (int, error)
}

// RetrievalResult pairs a memory with its relevance score from search.
type RetrievalResult struct {
	Memory    Memory  `json:"memory"`
	Relevance float64 `json:"relevance"`
}

// TouchRef identifies a single memory for the read-path access batcher.
//
// We carry UserID+ProjectID alongside ID because the hot tier's Redis
// key is `memory:<userID>:<projectID>:<id>` — without the prefix, the
// batcher can't reconstruct the key to bump access_count/last_accessed_at
// in hot. The cold UPDATE only needs ID (PK), so the cold path extracts
// IDs and ignores the rest.
type TouchRef struct {
	UserID    string
	ProjectID string
	ID        string
}

// TenantRef identifies a logical (user, project) bucket for cross-cutting
// operations like batch decay or distillation auto-discovery.
//
// Carrying a Count is optional and only populated by sources that already
// computed it (e.g. GROUP BY queries returning episodic counts). Callers
// that don't care can ignore it; sources that don't have it set 0.
type TenantRef struct {
	UserID    string
	ProjectID string
	// Count is the source-defined cardinality used for ranking — e.g. for
	// `ListActiveDistillTenants` this is "undistilled episodic count".
	// 0 means "not provided", not "zero", so don't filter on it.
	Count int
}

// CoreMemorySection represents a section of the core memory.
type CoreMemorySection struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	// Scope identifies which layer this section came from when sections are
	// merged across scopes (user_global vs. project). Empty on persisted
	// sections; populated by GetMerged so the prompt formatter can label
	// origin if desired. Not serialized for storage to keep on-disk schema
	// untouched.
	Scope CoreMemoryScope `json:"-"`
}

// CoreMemoryScope identifies the visibility layer of a core memory entry.
//
// ScopeProject (default) → entry visible only inside (userID, projectID).
// ScopeUser              → entry visible across *all* projects for userID;
//
//	useful for stable persona/preferences ("I prefer
//	Chinese responses", "I am a backend engineer").
//
// At read time, GetMerged stacks user scope under project scope so a
// project can override a global preference without mutating it.
type CoreMemoryScope string

const (
	CoreScopeProject CoreMemoryScope = "project"
	CoreScopeUser    CoreMemoryScope = "user"
)

// IsValid reports whether s is one of the recognized scope constants.
func (s CoreMemoryScope) IsValid() bool {
	return s == CoreScopeProject || s == CoreScopeUser
}

// CoreMemory represents the active context that is always provided to the agent.
type CoreMemory struct {
	UserID    string                        `json:"user_id"`
	ProjectID string                        `json:"project_id"`
	Sections  map[string]*CoreMemorySection `json:"sections"`
}

// CoreMemoryManager defines operations for managing core memory.
//
// The non-scoped methods (GetCoreMemory/AppendToSection/ReplaceInSection)
// operate on CoreScopeProject and are retained for backward compatibility
// — older call sites continue to work without modification.
//
// The *Scoped methods + GetMerged are the canonical entry points going
// forward; tools and prompt assembly should prefer them.
type CoreMemoryManager interface {
	GetCoreMemory(ctx context.Context, userID, projectID string) (*CoreMemory, error)
	AppendToSection(ctx context.Context, userID, projectID, section, content string) error
	ReplaceInSection(ctx context.Context, userID, projectID, section, oldContent, newContent string) error

	// GetCoreMemoryScoped reads a single scope (user or project).
	// For user scope, projectID is ignored.
	GetCoreMemoryScoped(ctx context.Context, userID, projectID string, scope CoreMemoryScope) (*CoreMemory, error)

	// AppendToSectionScoped / ReplaceInSectionScoped target a specific scope.
	AppendToSectionScoped(ctx context.Context, userID, projectID string, scope CoreMemoryScope, section, content string) error
	ReplaceInSectionScoped(ctx context.Context, userID, projectID string, scope CoreMemoryScope, section, oldContent, newContent string) error

	// GetMerged returns a CoreMemory whose Sections are the union of user
	// and project scope, with project sections overriding user sections of
	// the same name. Each returned *CoreMemorySection has Scope populated.
	GetMerged(ctx context.Context, userID, projectID string) (*CoreMemory, error)
}

