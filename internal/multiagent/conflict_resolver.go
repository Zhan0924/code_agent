package multiagent

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// FileEdit represents a proposed file modification from an agent.
type FileEdit struct {
	AgentID   string    `json:"agent_id"`
	FilePath  string    `json:"file_path"`
	Action    string    `json:"action"` // "write", "edit", "patch", "delete"
	Content   string    `json:"content,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// ConflictType classifies the nature of a file conflict.
type ConflictType string

const (
	ConflictConcurrentWrite ConflictType = "concurrent_write"
	ConflictDeleteModify    ConflictType = "delete_modify"
	ConflictOverlap        ConflictType = "overlap"
)

// Conflict describes a detected conflict between agent edits.
type Conflict struct {
	Type     ConflictType `json:"type"`
	FilePath string       `json:"file_path"`
	Edits    []FileEdit   `json:"edits"`
	Resolved bool         `json:"resolved"`
	Winner   string       `json:"winner,omitempty"` // agent ID of the winning edit
}

// ConflictResolver detects and resolves conflicts when multiple agents
// attempt to modify the same file concurrently.
type ConflictResolver struct {
	mu       sync.Mutex
	pending  map[string][]FileEdit // file_path → pending edits
	resolved []Conflict
	strategy ResolutionStrategy
	logger   *zap.Logger
}

// ResolutionStrategy determines how conflicts are resolved.
type ResolutionStrategy string

const (
	StrategyLastWriter  ResolutionStrategy = "last_writer"
	StrategyFirstWriter ResolutionStrategy = "first_writer"
	StrategyPriority    ResolutionStrategy = "priority" // based on agent type priority
)

// NewConflictResolver creates a resolver with the given strategy.
func NewConflictResolver(strategy ResolutionStrategy, logger *zap.Logger) *ConflictResolver {
	return &ConflictResolver{
		pending:  make(map[string][]FileEdit),
		strategy: strategy,
		logger:   logger.With(zap.String("component", "multiagent.conflict_resolver")),
	}
}

// RecordEdit registers a proposed file edit. Returns a conflict if one is detected.
func (r *ConflictResolver) RecordEdit(edit FileEdit) *Conflict {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing := r.pending[edit.FilePath]

	// Check for conflicts with pending edits from other agents
	for _, prev := range existing {
		if prev.AgentID == edit.AgentID {
			continue
		}
		if isConflicting(prev, edit) {
			conflict := &Conflict{
				Type:     detectConflictType(prev, edit),
				FilePath: edit.FilePath,
				Edits:    []FileEdit{prev, edit},
			}
			r.logger.Warn("conflict detected",
				zap.String("file", edit.FilePath),
				zap.String("agent_a", prev.AgentID),
				zap.String("agent_b", edit.AgentID),
				zap.String("type", string(conflict.Type)))
			return conflict
		}
	}

	r.pending[edit.FilePath] = append(existing, edit)
	return nil
}

// Resolve applies the configured strategy to pick a winner.
func (r *ConflictResolver) Resolve(conflict *Conflict) FileEdit {
	r.mu.Lock()
	defer r.mu.Unlock()

	var winner FileEdit
	switch r.strategy {
	case StrategyFirstWriter:
		winner = conflict.Edits[0]
	case StrategyPriority:
		winner = r.resolveByPriority(conflict.Edits)
	default: // StrategyLastWriter
		winner = conflict.Edits[len(conflict.Edits)-1]
	}

	conflict.Resolved = true
	conflict.Winner = winner.AgentID
	r.resolved = append(r.resolved, *conflict)

	// Remove losing edits from pending
	r.pending[conflict.FilePath] = []FileEdit{winner}

	r.logger.Info("conflict resolved",
		zap.String("file", conflict.FilePath),
		zap.String("winner", winner.AgentID),
		zap.String("strategy", string(r.strategy)))

	return winner
}

// CommitEdit marks an edit as successfully applied and clears it from pending.
func (r *ConflictResolver) CommitEdit(filePath, agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	edits := r.pending[filePath]
	filtered := edits[:0]
	for _, e := range edits {
		if e.AgentID != agentID {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == 0 {
		delete(r.pending, filePath)
	} else {
		r.pending[filePath] = filtered
	}
}

// PendingConflicts returns all unresolved file paths with multiple pending edits.
func (r *ConflictResolver) PendingConflicts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var paths []string
	for path, edits := range r.pending {
		agents := make(map[string]bool)
		for _, e := range edits {
			agents[e.AgentID] = true
		}
		if len(agents) > 1 {
			paths = append(paths, path)
		}
	}
	return paths
}

// ResolvedCount returns the number of conflicts that have been resolved.
func (r *ConflictResolver) ResolvedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.resolved)
}

func (r *ConflictResolver) resolveByPriority(edits []FileEdit) FileEdit {
	// Priority: code > test > review (code agents' edits take precedence)
	priority := map[string]int{
		"code": 3, "test": 2, "review": 1,
	}

	best := edits[0]
	bestPri := 0
	for _, e := range edits {
		agentType := extractAgentType(e.AgentID)
		if p := priority[agentType]; p > bestPri {
			bestPri = p
			best = e
		}
	}
	return best
}

func extractAgentType(agentID string) string {
	parts := strings.SplitN(agentID, "-", 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func isConflicting(a, b FileEdit) bool {
	if a.Action == "delete" || b.Action == "delete" {
		return true
	}
	// Two writes to the same file from different agents = conflict
	return true
}

func detectConflictType(a, b FileEdit) ConflictType {
	if a.Action == "delete" || b.Action == "delete" {
		return ConflictDeleteModify
	}
	return ConflictConcurrentWrite
}

// FormatConflictReport generates a summary of a conflict for logging or LLM context.
func FormatConflictReport(conflict *Conflict) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Conflict] File: %s, Type: %s\n", conflict.FilePath, conflict.Type))
	for _, e := range conflict.Edits {
		sb.WriteString(fmt.Sprintf("  - Agent %s: %s at %s\n", e.AgentID, e.Action, e.Timestamp.Format(time.RFC3339)))
	}
	if conflict.Resolved {
		sb.WriteString(fmt.Sprintf("  Winner: %s\n", conflict.Winner))
	}
	return sb.String()
}
