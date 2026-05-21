// Package orchestrator: edit_engine.go implements a precision code edit engine
// inspired by Claude Code's "unique string match" strategy.
//
// Key features:
//   - old_text must match exactly once in the file (rejects 0 or 2+ matches)
//   - Automatic backup before edit (.bak file)
//   - Post-edit lint/compile check with auto-rollback on failure
//   - Multi-file atomic transactions (all-or-nothing)
//   - Unified diff preview for UI display
package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/workspace"
	"go.uber.org/zap"
)

// ─── Edit Engine ────────────────────────────────────────────────────────────

// EditEngine provides safe, validated code editing with backup and rollback.
type EditEngine struct {
	workspaceMgr *workspace.Manager
	lintChecker  *LintChecker
	logger       *zap.Logger

	// pathLocks serialises edits per file. Without this, two concurrent
	// ApplyEdit calls on the same path can each pass the uniqueness check
	// against the pre-edit content, both write, and silently lose one
	// caller's change. The map is keyed by the canonical absolute path so
	// different relative spellings (./foo vs foo) still contend correctly.
	pathLocksMu sync.Mutex
	pathLocks   map[string]*sync.Mutex
}

// NewEditEngine creates a new EditEngine with lint checking capabilities.
func NewEditEngine(wm *workspace.Manager, logger *zap.Logger) *EditEngine {
	return &EditEngine{
		workspaceMgr: wm,
		lintChecker:  NewLintChecker(logger),
		pathLocks:    make(map[string]*sync.Mutex),
		logger:       logger.With(zap.String("component", "edit_engine")),
	}
}

// lockPath returns (and acquires) the mutex that guards edits to the given
// absolute path. The returned unlock func must be called — typically via defer
// — exactly once. Lookups are O(1) under a small mutex; acquisitions of the
// returned per-path mutex can block, which is the whole point.
func (e *EditEngine) lockPath(absPath string) (unlock func()) {
	e.pathLocksMu.Lock()
	mu, ok := e.pathLocks[absPath]
	if !ok {
		mu = &sync.Mutex{}
		e.pathLocks[absPath] = mu
	}
	e.pathLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// lockPaths acquires per-path mutexes for every path in ops in a deterministic
// order (sorted) to avoid deadlocks when two multi-edits overlap. Returns a
// single unlock func that releases them in reverse order.
func (e *EditEngine) lockPaths(ws *workspace.Workspace, ops []EditOperation) func() {
	seen := make(map[string]struct{}, len(ops))
	paths := make([]string, 0, len(ops))
	for _, op := range ops {
		abs := filepath.Join(ws.RootDir, op.Path)
		if _, dup := seen[abs]; dup {
			continue
		}
		seen[abs] = struct{}{}
		paths = append(paths, abs)
	}
	sort.Strings(paths)

	unlocks := make([]func(), 0, len(paths))
	for _, p := range paths {
		unlocks = append(unlocks, e.lockPath(p))
	}
	return func() {
		// Release in reverse acquisition order (LIFO).
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

// EditOperation represents a single file edit operation.
type EditOperation struct {
	Path    string `json:"path"`     // Relative file path within workspace
	OldText string `json:"old_text"` // Must match exactly once in the file
	NewText string `json:"new_text"` // Replacement text
}

// EditResult contains the outcome of an edit operation.
type EditResult struct {
	Success     bool     `json:"success"`
	FilePath    string   `json:"file_path"`
	DiffPreview string   `json:"diff_preview"` // Unified diff for UI display
	BackupPath  string   `json:"backup_path"`  // Path to .bak file (for manual rollback)
	LintErrors  []string `json:"lint_errors"`  // Post-edit lint errors (empty = clean)
	RolledBack  bool     `json:"rolled_back"`  // True if auto-rollback occurred
	Message     string   `json:"message"`      // Human-readable summary
}

// ApplyEdit performs a single precision edit with uniqueness validation, backup, and lint.
func (e *EditEngine) ApplyEdit(ctx context.Context, ws *workspace.Workspace, op EditOperation) *EditResult {
	result := &EditResult{FilePath: op.Path}

	// Serialise concurrent edits to the same file. Without this, two callers
	// could both read identical pre-edit content, both pass the uniqueness
	// check, and both write — the second write silently overwrites the first.
	absPath := filepath.Join(ws.RootDir, op.Path)
	defer e.lockPath(absPath)()

	// Step 1: Read existing file content
	existing, err := e.workspaceMgr.ReadFile(ws, op.Path)
	if err != nil {
		result.Message = fmt.Sprintf("Failed to read '%s': %v", op.Path, err)
		return result
	}

	// Step 2: Validate old_text uniqueness
	matchCount := strings.Count(existing, op.OldText)
	switch matchCount {
	case 0:
		result.Message = fmt.Sprintf("❌ edit_file failed: old_text not found in '%s'.\n\n"+
			"The text you're trying to replace doesn't exist in the file. "+
			"Please use read_file to get the current content first, then provide the exact text to replace.\n\n"+
			"Hint: Make sure whitespace, indentation, and line endings match exactly.", op.Path)
		return result
	case 1:
		// Perfect — exactly one match, proceed
	default:
		result.Message = fmt.Sprintf("❌ edit_file failed: old_text matches %d times in '%s'.\n\n"+
			"The text you're trying to replace is ambiguous (found %d occurrences). "+
			"Please include more surrounding context lines to make the match unique.\n\n"+
			"Tip: Include the function signature, comment, or nearby lines to disambiguate.", matchCount, op.Path, matchCount)
		return result
	}

	// Step 3: Create backup
	backupPath := op.Path + ".bak"
	if err := e.workspaceMgr.WriteFile(ws, backupPath, existing); err != nil {
		e.logger.Warn("failed to create backup", zap.String("path", backupPath), zap.Error(err))
		// Continue anyway — backup is best-effort
	}
	result.BackupPath = backupPath

	// Step 4: Apply the edit
	patched := strings.Replace(existing, op.OldText, op.NewText, 1)
	if err := e.workspaceMgr.WriteFile(ws, op.Path, patched); err != nil {
		result.Message = fmt.Sprintf("Failed to write patched '%s': %v", op.Path, err)
		return result
	}

	// Step 5: Generate unified diff preview
	result.DiffPreview = generateUnifiedDiff(op.Path, existing, patched)

	// Step 6: Post-edit lint/compile check
	lintErrors := e.lintChecker.Check(ctx, absPath, ws.RootDir)
	result.LintErrors = lintErrors

	if len(lintErrors) > 0 {
		// Auto-rollback: restore from backup
		e.logger.Warn("lint failed after edit, rolling back",
			zap.String("path", op.Path),
			zap.Strings("errors", lintErrors))

		if err := e.workspaceMgr.WriteFile(ws, op.Path, existing); err != nil {
			e.logger.Error("rollback failed!", zap.String("path", op.Path), zap.Error(err))
			result.Message = fmt.Sprintf("⚠️ CRITICAL: Edit caused lint errors AND rollback failed for '%s'.\n"+
				"Lint errors: %s\nRollback error: %v\nBackup is at: %s",
				op.Path, strings.Join(lintErrors, "\n"), err, backupPath)
			return result
		}

		result.RolledBack = true
		result.Message = fmt.Sprintf("❌ Edit rolled back for '%s' — post-edit lint/compile check failed:\n%s\n\n"+
			"The file has been restored to its original state. Please fix the issues and try again.\n"+
			"Diff of the attempted (failed) edit:\n%s",
			op.Path, strings.Join(lintErrors, "\n"), result.DiffPreview)
		return result
	}

	// Clean up backup on success
	_ = e.workspaceMgr.DeleteFile(ws, backupPath)

	result.Success = true
	result.Message = fmt.Sprintf("✅ Successfully edited '%s' (replaced %d chars → %d chars, lint passed)\n\nDiff:\n%s",
		op.Path, len(op.OldText), len(op.NewText), result.DiffPreview)
	return result
}

// ApplyMultiEdit performs multiple edits atomically — if any lint check fails,
// all edits are rolled back.
func (e *EditEngine) ApplyMultiEdit(ctx context.Context, ws *workspace.Workspace, ops []EditOperation) []*EditResult {
	// Acquire all per-path locks up front in a deterministic order to prevent
	// deadlocks with another overlapping multi-edit. This also blocks any
	// single ApplyEdit that tries to touch one of our paths while we run.
	defer e.lockPaths(ws, ops)()

	results := make([]*EditResult, len(ops))
	backups := make(map[string]string) // path → original content

	// Phase 1: Validate all edits and create backups
	for i, op := range ops {
		existing, err := e.workspaceMgr.ReadFile(ws, op.Path)
		if err != nil {
			results[i] = &EditResult{
				FilePath: op.Path,
				Message:  fmt.Sprintf("Failed to read '%s': %v", op.Path, err),
			}
			// Rollback any previously applied edits
			e.rollbackAll(ws, backups)
			return results
		}
		backups[op.Path] = existing

		matchCount := strings.Count(existing, op.OldText)
		if matchCount != 1 {
			results[i] = &EditResult{
				FilePath: op.Path,
				Message:  fmt.Sprintf("old_text matches %d times (need exactly 1) in '%s'", matchCount, op.Path),
			}
			e.rollbackAll(ws, backups)
			return results
		}
	}

	// Phase 2: Apply all edits
	for i, op := range ops {
		existing := backups[op.Path]
		patched := strings.Replace(existing, op.OldText, op.NewText, 1)
		if err := e.workspaceMgr.WriteFile(ws, op.Path, patched); err != nil {
			results[i] = &EditResult{
				FilePath: op.Path,
				Message:  fmt.Sprintf("Failed to write '%s': %v", op.Path, err),
			}
			e.rollbackAll(ws, backups)
			return results
		}
		results[i] = &EditResult{
			FilePath:    op.Path,
			Success:     true,
			DiffPreview: generateUnifiedDiff(op.Path, existing, patched),
		}
	}

	// Phase 3: Lint check all edited files
	allClean := true
	for i, op := range ops {
		absPath := filepath.Join(ws.RootDir, op.Path)
		lintErrors := e.lintChecker.Check(ctx, absPath, ws.RootDir)
		results[i].LintErrors = lintErrors
		if len(lintErrors) > 0 {
			allClean = false
			results[i].Success = false
			results[i].Message = fmt.Sprintf("Lint errors in '%s': %s", op.Path, strings.Join(lintErrors, "; "))
		}
	}

	if !allClean {
		// Rollback everything
		e.rollbackAll(ws, backups)
		for i := range results {
			results[i].RolledBack = true
			if results[i].Message == "" {
				results[i].Message = "Rolled back due to lint errors in other files"
			}
		}
	} else {
		for i, op := range ops {
			results[i].Message = fmt.Sprintf("✅ Successfully edited '%s'", op.Path)
		}
	}

	return results
}

func (e *EditEngine) rollbackAll(ws *workspace.Workspace, backups map[string]string) {
	for path, content := range backups {
		if err := e.workspaceMgr.WriteFile(ws, path, content); err != nil {
			e.logger.Error("rollback failed", zap.String("path", path), zap.Error(err))
		}
	}
}

// ─── Lint Checker ───────────────────────────────────────────────────────────

// LintChecker runs language-specific linting/compilation checks on files.
type LintChecker struct {
	logger *zap.Logger
}

// NewLintChecker creates a new LintChecker.
func NewLintChecker(logger *zap.Logger) *LintChecker {
	return &LintChecker{logger: logger.With(zap.String("component", "lint_checker"))}
}

// Check runs the appropriate lint/compile check for the given file.
// Returns a slice of error strings (empty means file is clean).
func (lc *LintChecker) Check(ctx context.Context, absFilePath string, workspaceRoot string) []string {
	ext := strings.ToLower(filepath.Ext(absFilePath))

	switch ext {
	case ".go":
		return lc.checkGo(ctx, absFilePath, workspaceRoot)
	case ".py":
		return lc.checkPython(ctx, absFilePath, workspaceRoot)
	case ".ts", ".tsx":
		return lc.checkTypeScript(ctx, absFilePath, workspaceRoot)
	case ".js", ".jsx":
		return lc.checkJavaScript(ctx, absFilePath, workspaceRoot)
	case ".rs":
		return lc.checkRust(ctx, workspaceRoot)
	default:
		// No lint checker available for this file type
		return nil
	}
}

func (lc *LintChecker) checkGo(ctx context.Context, filePath string, workspaceRoot string) []string {
	// Check if go.mod exists — if not, skip (not a Go module)
	if _, err := os.Stat(filepath.Join(workspaceRoot, "go.mod")); os.IsNotExist(err) {
		return nil
	}

	// Run 'go vet' on the package containing the file
	pkgDir := filepath.Dir(filePath)
	relPkg, err := filepath.Rel(workspaceRoot, pkgDir)
	if err != nil {
		relPkg = "./..."
	} else {
		relPkg = "./" + relPkg
	}

	return lc.runLintCmd(ctx, workspaceRoot, "go", "vet", relPkg)
}

func (lc *LintChecker) checkPython(ctx context.Context, filePath string, workspaceRoot string) []string {
	// Try ruff first (fast), fall back to python -m py_compile
	if _, err := exec.LookPath("ruff"); err == nil {
		return lc.runLintCmd(ctx, workspaceRoot, "ruff", "check", "--select=E", filePath)
	}
	// Fallback: basic syntax check
	return lc.runLintCmd(ctx, workspaceRoot, "python3", "-m", "py_compile", filePath)
}

func (lc *LintChecker) checkTypeScript(ctx context.Context, filePath string, workspaceRoot string) []string {
	// Check if tsconfig.json exists
	if _, err := os.Stat(filepath.Join(workspaceRoot, "tsconfig.json")); os.IsNotExist(err) {
		return nil // no TS project configured
	}
	return lc.runLintCmd(ctx, workspaceRoot, "npx", "tsc", "--noEmit", "--pretty", "false")
}

func (lc *LintChecker) checkJavaScript(ctx context.Context, filePath string, workspaceRoot string) []string {
	// Basic syntax check with node
	return lc.runLintCmd(ctx, workspaceRoot, "node", "--check", filePath)
}

func (lc *LintChecker) checkRust(ctx context.Context, workspaceRoot string) []string {
	if _, err := os.Stat(filepath.Join(workspaceRoot, "Cargo.toml")); os.IsNotExist(err) {
		return nil
	}
	return lc.runLintCmd(ctx, workspaceRoot, "cargo", "check", "--message-format=short")
}

// runLintCmd executes a lint command and returns error lines (nil if clean).
func (lc *LintChecker) runLintCmd(ctx context.Context, workDir string, name string, args ...string) []string {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, name, args...)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return nil // lint passed
	}

	// Collect error output
	output := stderr.String()
	if output == "" {
		output = stdout.String()
	}
	if output == "" {
		output = err.Error()
	}

	// Parse into individual error lines (skip empty lines)
	var errors []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			errors = append(errors, line)
			if len(errors) >= 10 {
				errors = append(errors, "... (more errors truncated)")
				break
			}
		}
	}

	if len(errors) == 0 {
		errors = append(errors, fmt.Sprintf("lint command failed: %v", err))
	}

	lc.logger.Debug("lint check failed",
		zap.String("cmd", name+" "+strings.Join(args, " ")),
		zap.Int("errors", len(errors)))

	return errors
}

// ─── Diff Generation ────────────────────────────────────────────────────────

// generateUnifiedDiff creates a unified diff between old and new content for
// display. It emits a single hunk spanning the change region plus 3 lines of
// context. The line counts in the @@ header are computed from the actual
// hunk body (context-before + removed + context-after, context-before + added
// + context-after) rather than the prior implementation's tail-aligned
// offset heuristic, which produced malformed diffs whenever the number of
// inserted lines differed from the number of removed lines.
func generateUnifiedDiff(filePath, oldContent, newContent string) string {
	if oldContent == newContent {
		return "(no diff — files are identical)"
	}

	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	// Find the first line that differs.
	firstDiff := 0
	for firstDiff < len(oldLines) && firstDiff < len(newLines) &&
		oldLines[firstDiff] == newLines[firstDiff] {
		firstDiff++
	}

	// Find the last line that differs, counted from the end of each slice.
	// We stop when we've walked past the common prefix either side. lastOld
	// is the index (exclusive) one past the last changed old line; lastNew
	// likewise for new. By symmetry, tailMatch counts identical trailing
	// lines shared by both.
	tailMatch := 0
	for tailMatch < len(oldLines)-firstDiff && tailMatch < len(newLines)-firstDiff {
		if oldLines[len(oldLines)-1-tailMatch] != newLines[len(newLines)-1-tailMatch] {
			break
		}
		tailMatch++
	}
	lastOld := len(oldLines) - tailMatch // exclusive
	lastNew := len(newLines) - tailMatch // exclusive
	// Guard against lastOld < firstDiff (can happen when only additions occur
	// at the head and the whole original is shared in the tail).
	if lastOld < firstDiff {
		lastOld = firstDiff
	}
	if lastNew < firstDiff {
		lastNew = firstDiff
	}

	const contextLines = 3
	start := firstDiff - contextLines
	if start < 0 {
		start = 0
	}
	endOld := lastOld + contextLines
	if endOld > len(oldLines) {
		endOld = len(oldLines)
	}
	endNew := lastNew + contextLines
	if endNew > len(newLines) {
		endNew = len(newLines)
	}

	var diff strings.Builder
	diff.WriteString(fmt.Sprintf("--- a/%s\n+++ b/%s\n", filePath, filePath))

	// Hunk header: counts are the exact number of lines we're about to emit
	// for each side (context-before + changed + context-after). Line numbers
	// are 1-based and begin at start+1.
	oldCount := endOld - start
	newCount := endNew - start
	diff.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", start+1, oldCount, start+1, newCount))

	// Context before the change.
	for i := start; i < firstDiff; i++ {
		diff.WriteString(" " + oldLines[i] + "\n")
	}
	// Removed lines.
	for i := firstDiff; i < lastOld; i++ {
		diff.WriteString("-" + oldLines[i] + "\n")
	}
	// Added lines.
	for i := firstDiff; i < lastNew; i++ {
		diff.WriteString("+" + newLines[i] + "\n")
	}
	// Context after the change (taken from the new side — identical to old by
	// definition of the tail-match above).
	for i := lastNew; i < endNew; i++ {
		diff.WriteString(" " + newLines[i] + "\n")
	}

	result := diff.String()
	if len(result) > 5000 {
		result = result[:5000] + "\n... [diff truncated at 5000 chars]"
	}
	return result
}
