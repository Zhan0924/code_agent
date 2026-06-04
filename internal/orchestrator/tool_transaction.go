package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ToolTransaction tracks file modifications during a ReAct loop iteration,
// enabling rollback on interrupt. It extends the EditEngine's per-file .bak
// mechanism to a session-level dirty state tracker.
type ToolTransaction struct {
	mu         sync.Mutex
	sessionID  string
	dirtyFiles map[string]fileSnapshot
	startedAt  time.Time
	logger     *zap.Logger
}

type fileSnapshot struct {
	Path       string
	Content    []byte
	Existed    bool
	CapturedAt time.Time
}

// NewToolTransaction creates a transaction tracker for a session.
func NewToolTransaction(sessionID string, logger *zap.Logger) *ToolTransaction {
	return &ToolTransaction{
		sessionID:  sessionID,
		dirtyFiles: make(map[string]fileSnapshot),
		startedAt:  time.Now(),
		logger:     logger.With(zap.String("component", "tool_transaction"), zap.String("session", sessionID)),
	}
}

// CaptureBeforeWrite records the original state of a file before modification.
// Must be called before any write/edit/patch operation on the file.
func (t *ToolTransaction) CaptureBeforeWrite(absPath string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, already := t.dirtyFiles[absPath]; already {
		return
	}

	snap := fileSnapshot{
		Path:       absPath,
		CapturedAt: time.Now(),
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		snap.Existed = false
	} else {
		snap.Existed = true
		snap.Content = content
	}

	t.dirtyFiles[absPath] = snap
}

// Rollback reverts all dirty files to their pre-transaction state.
// Returns the number of files rolled back and any errors encountered.
func (t *ToolTransaction) Rollback() (int, []error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var errs []error
	rolled := 0

	for path, snap := range t.dirtyFiles {
		if !snap.Existed {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
			} else {
				rolled++
			}
		} else {
			dir := filepath.Dir(path)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				errs = append(errs, fmt.Errorf("mkdir %s: %w", dir, err))
				continue
			}
			if err := os.WriteFile(path, snap.Content, 0o644); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", path, err))
			} else {
				rolled++
			}
		}
	}

	t.logger.Info("transaction rolled back",
		zap.Int("files_rolled", rolled),
		zap.Int("errors", len(errs)))

	t.dirtyFiles = make(map[string]fileSnapshot)
	return rolled, errs
}

// DirtyFiles returns the list of files modified in this transaction.
func (t *ToolTransaction) DirtyFiles() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	files := make([]string, 0, len(t.dirtyFiles))
	for path := range t.dirtyFiles {
		files = append(files, path)
	}
	return files
}

// Clear resets the transaction state (e.g., after successful commit).
func (t *ToolTransaction) Clear() {
	t.mu.Lock()
	t.dirtyFiles = make(map[string]fileSnapshot)
	t.mu.Unlock()
}

// HasDirtyFiles returns true if any files have been modified.
func (t *ToolTransaction) HasDirtyFiles() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.dirtyFiles) > 0
}

// registerTx creates a ToolTransaction for sessionID, registers it in
// o.txMap so CaptureBeforeWrite (orchestrator.go:1681-1685 iterates the map)
// can populate it, and returns a release closure for the caller's defer.
//
// Extracted from reactLoop:447-456 so the streaming path
// (ProcessMessageStreamFull) can also participate. Before this refactor the
// streaming path never registered a transaction, so file writes on that path
// could not be rolled back on interrupt.
func (o *Orchestrator) registerTx(sessionID string) (tx *ToolTransaction, release func()) {
	tx = NewToolTransaction(sessionID, o.logger)
	o.txMu.Lock()
	o.txMap[sessionID] = tx
	o.txMu.Unlock()
	release = func() {
		o.txMu.Lock()
		delete(o.txMap, sessionID)
		o.txMu.Unlock()
	}
	return tx, release
}
