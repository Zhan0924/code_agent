package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agent/code_agent/internal/workspace"
	"go.uber.org/zap"
)

func setupTestWorkspace(t *testing.T) (*workspace.Manager, *workspace.Workspace) {
	t.Helper()
	dir := t.TempDir()
	logger := zap.NewNop()
	wm, err := workspace.NewManager(dir, logger)
	if err != nil {
		t.Fatalf("failed to create workspace manager: %v", err)
	}
	ws, err := wm.Create("test", "test-project")
	if err != nil {
		t.Fatalf("failed to create workspace: %v", err)
	}
	return wm, ws
}

func TestEditEngine_UniqueMatch_Success(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	engine := NewEditEngine(wm, logger)

	// Write a file with unique content
	content := `package main

func hello() string {
	return "hello"
}

func world() string {
	return "world"
}
`
	if err := wm.WriteFile(ws, "main.go", content); err != nil {
		t.Fatal(err)
	}

	// Edit with unique match — no go.mod, so lint will be skipped
	result := engine.ApplyEdit(context.Background(), ws, EditOperation{
		Path:    "main.go",
		OldText: `return "hello"`,
		NewText: `return "hi"`,
	})

	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Message)
	}

	// Verify file was actually changed
	updated, _ := wm.ReadFile(ws, "main.go")
	if !contains(updated, `return "hi"`) {
		t.Errorf("file was not updated: %s", updated)
	}
	if contains(updated, `return "hello"`) {
		t.Errorf("old text still present: %s", updated)
	}

	// Verify diff preview is generated
	if result.DiffPreview == "" {
		t.Error("expected diff preview, got empty")
	}
}

func TestEditEngine_ZeroMatches(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	engine := NewEditEngine(wm, logger)

	if err := wm.WriteFile(ws, "test.go", "package main\n"); err != nil {
		t.Fatal(err)
	}

	result := engine.ApplyEdit(context.Background(), ws, EditOperation{
		Path:    "test.go",
		OldText: "nonexistent text",
		NewText: "replacement",
	})

	if result.Success {
		t.Fatal("expected failure for zero matches")
	}
	if !contains(result.Message, "not found") {
		t.Errorf("expected 'not found' message, got: %s", result.Message)
	}
}

func TestEditEngine_MultipleMatches(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	engine := NewEditEngine(wm, logger)

	// File with duplicate text
	content := `func a() { return nil }
func b() { return nil }
func c() { return nil }
`
	if err := wm.WriteFile(ws, "dup.go", content); err != nil {
		t.Fatal(err)
	}

	result := engine.ApplyEdit(context.Background(), ws, EditOperation{
		Path:    "dup.go",
		OldText: "return nil",
		NewText: "return err",
	})

	if result.Success {
		t.Fatal("expected failure for multiple matches")
	}
	if !contains(result.Message, "matches 3 times") {
		t.Errorf("expected multiple match message, got: %s", result.Message)
	}

	// Verify file was NOT modified
	unchanged, _ := wm.ReadFile(ws, "dup.go")
	if unchanged != content {
		t.Error("file should not be modified when match is ambiguous")
	}
}

func TestEditEngine_BackupCreated(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	engine := NewEditEngine(wm, logger)

	original := "package main\nfunc unique123() {}\n"
	if err := wm.WriteFile(ws, "backup.go", original); err != nil {
		t.Fatal(err)
	}

	// The edit will succeed (no lint since no go.mod)
	engine.ApplyEdit(context.Background(), ws, EditOperation{
		Path:    "backup.go",
		OldText: "unique123",
		NewText: "unique456",
	})

	// Backup should be cleaned up on success (no lint errors)
	backupPath := filepath.Join(ws.RootDir, "backup.go.bak")
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Error("backup file should be cleaned up after successful edit")
	}
}

func TestEditEngine_MultiEdit_Atomic(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	engine := NewEditEngine(wm, logger)

	// Create two files
	if err := wm.WriteFile(ws, "a.txt", "hello world\n"); err != nil {
		t.Fatal(err)
	}
	if err := wm.WriteFile(ws, "b.txt", "foo bar baz\n"); err != nil {
		t.Fatal(err)
	}

	results := engine.ApplyMultiEdit(context.Background(), ws, []EditOperation{
		{Path: "a.txt", OldText: "hello", NewText: "hi"},
		{Path: "b.txt", OldText: "foo", NewText: "qux"},
	})

	for i, r := range results {
		if !r.Success {
			t.Errorf("edit %d failed: %s", i, r.Message)
		}
	}

	// Verify both files changed
	a, _ := wm.ReadFile(ws, "a.txt")
	b, _ := wm.ReadFile(ws, "b.txt")
	if !contains(a, "hi") {
		t.Errorf("a.txt not updated: %s", a)
	}
	if !contains(b, "qux") {
		t.Errorf("b.txt not updated: %s", b)
	}
}

func TestEditEngine_MultiEdit_RollbackOnValidationFailure(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	engine := NewEditEngine(wm, logger)

	original := "unique content here\n"
	if err := wm.WriteFile(ws, "good.txt", original); err != nil {
		t.Fatal(err)
	}
	if err := wm.WriteFile(ws, "bad.txt", "some text\n"); err != nil {
		t.Fatal(err)
	}

	results := engine.ApplyMultiEdit(context.Background(), ws, []EditOperation{
		{Path: "good.txt", OldText: "unique content", NewText: "new content"},
		{Path: "bad.txt", OldText: "NONEXISTENT", NewText: "replacement"}, // will fail validation
	})

	// Second edit should fail
	if results[1] == nil || results[1].Success {
		t.Error("expected second edit to fail")
	}

	// First file should be rolled back
	content, _ := wm.ReadFile(ws, "good.txt")
	if content != original {
		t.Errorf("good.txt should be rolled back, got: %s", content)
	}
}

func TestGenerateUnifiedDiff(t *testing.T) {
	old := "line1\nline2\nline3\nline4\nline5\n"
	new := "line1\nline2\nLINE3_CHANGED\nline4\nline5\n"

	diff := generateUnifiedDiff("test.txt", old, new)

	if !contains(diff, "--- a/test.txt") {
		t.Error("diff should have file header")
	}
	if !contains(diff, "-line3") {
		t.Error("diff should show removed line")
	}
	if !contains(diff, "+LINE3_CHANGED") {
		t.Error("diff should show added line")
	}
}

func TestGenerateUnifiedDiff_Identical(t *testing.T) {
	content := "same\ncontent\n"
	diff := generateUnifiedDiff("test.txt", content, content)
	if !contains(diff, "identical") {
		t.Error("diff of identical files should mention 'identical'")
	}
}

// ─── LintChecker Tests ─────────────────────────────────────────────────────

func TestLintChecker_NoCheckerForExtension(t *testing.T) {
	logger := zap.NewNop()
	lc := NewLintChecker(logger)

	errors := lc.Check(context.Background(), "/tmp/test.yaml", "/tmp")
	if errors != nil {
		t.Errorf("expected nil for unknown extension, got: %v", errors)
	}
}

func TestLintChecker_GoVet_NoGoMod(t *testing.T) {
	logger := zap.NewNop()
	lc := NewLintChecker(logger)

	dir := t.TempDir()
	// No go.mod in dir — should skip
	errors := lc.checkGo(context.Background(), filepath.Join(dir, "main.go"), dir)
	if errors != nil {
		t.Errorf("expected nil when no go.mod, got: %v", errors)
	}
}

// helper
func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (s == substr || len(s) > len(substr) && containsImpl(s, substr))
}

func containsImpl(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestEditEngine_ConcurrentEditsSerialized verifies that two ApplyEdit calls
// against the same file do not race. Before per-path locking was added, both
// callers could pass the uniqueness check on identical pre-edit content, both
// Write, and the second write silently overwrites the first. After the fix,
// edits are serialised: one wins with success, the other sees the first's
// result and reports old_text not found.
func TestEditEngine_ConcurrentEditsSerialized(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	engine := NewEditEngine(wm, zap.NewNop())

	if err := wm.WriteFile(ws, "f.txt", "alpha\nbeta\n"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]*EditResult, 2)
	edits := []EditOperation{
		{Path: "f.txt", OldText: "alpha", NewText: "A"},
		{Path: "f.txt", OldText: "alpha", NewText: "B"},
	}
	for i, op := range edits {
		wg.Add(1)
		go func(i int, op EditOperation) {
			defer wg.Done()
			results[i] = engine.ApplyEdit(context.Background(), ws, op)
		}(i, op)
	}
	wg.Wait()

	successCount := 0
	for _, r := range results {
		if r.Success {
			successCount++
		}
	}
	if successCount != 1 {
		t.Errorf("expected exactly one edit to succeed (the other must see old_text already replaced), got %d successes", successCount)
	}

	final, err := wm.ReadFile(ws, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if final != "A\nbeta\n" && final != "B\nbeta\n" {
		t.Errorf("final content must equal one of the edits, got: %q", final)
	}
}

// TestUnifiedDiff_InsertsNotEqualDeletes asserts that the hunk header line
// counts are correct when insertions and deletions differ — the prior
// tail-alignment heuristic produced malformed headers here.
func TestUnifiedDiff_InsertsNotEqualDeletes(t *testing.T) {
	old := "a\nb\nc\n"
	// Replace the single line "b" with two lines "B1\nB2".
	new := "a\nB1\nB2\nc\n"
	diff := generateUnifiedDiff("f.txt", old, new)

	if !strings.Contains(diff, "@@ -1,3 +1,4 @@") {
		// strings.Split with trailing \n yields 4 old lines (incl. empty
		// after final \n) and 5 new lines; context is 3, so old count = 4
		// and new count = 5. Accept either formula — the critical property
		// is that the header's line count matches the emitted body.
		headerOK := false
		for _, line := range strings.Split(diff, "\n") {
			if strings.HasPrefix(line, "@@") {
				// Parse and cross-check.
				headerOK = validateHunkHeader(t, line, diff)
				break
			}
		}
		if !headerOK {
			t.Errorf("hunk header inconsistent with body; diff:\n%s", diff)
		}
	}
	if !strings.Contains(diff, "-b") {
		t.Errorf("missing '-b' line in diff:\n%s", diff)
	}
	if !strings.Contains(diff, "+B1") || !strings.Contains(diff, "+B2") {
		t.Errorf("missing added lines in diff:\n%s", diff)
	}
}

// validateHunkHeader parses the @@ -X,Y +A,B @@ line and confirms the Y and
// B counts match the number of context+removed and context+added body lines.
func validateHunkHeader(t *testing.T, header, fullDiff string) bool {
	t.Helper()
	var oldStart, oldCount, newStart, newCount int
	if _, err := parseHunk(header, &oldStart, &oldCount, &newStart, &newCount); err != nil {
		t.Logf("parse hunk header: %v", err)
		return false
	}

	bodyOld, bodyNew := 0, 0
	afterHeader := false
	for _, line := range strings.Split(fullDiff, "\n") {
		if !afterHeader {
			if strings.HasPrefix(line, "@@") {
				afterHeader = true
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, " "):
			bodyOld++
			bodyNew++
		case strings.HasPrefix(line, "-"):
			bodyOld++
		case strings.HasPrefix(line, "+"):
			bodyNew++
		}
	}
	if bodyOld != oldCount || bodyNew != newCount {
		t.Errorf("header says old=%d new=%d, body has old=%d new=%d",
			oldCount, newCount, bodyOld, bodyNew)
		return false
	}
	return true
}

// parseHunk extracts the four numbers from a unified-diff hunk header.
func parseHunk(header string, oldStart, oldCount, newStart, newCount *int) (int, error) {
	// header example: "@@ -1,3 +1,4 @@"
	trimmed := strings.TrimPrefix(header, "@@ ")
	trimmed = strings.TrimSuffix(trimmed, " @@")
	parts := strings.SplitN(trimmed, " ", 2)
	if len(parts) != 2 {
		return 0, &hunkParseErr{header}
	}
	if n, err := parsePair(parts[0], "-", oldStart, oldCount); err != nil {
		return n, err
	}
	return parsePair(parts[1], "+", newStart, newCount)
}

type hunkParseErr struct{ s string }

func (e *hunkParseErr) Error() string { return "bad hunk header: " + e.s }

func parsePair(s, prefix string, start, count *int) (int, error) {
	s = strings.TrimPrefix(s, prefix)
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return 0, &hunkParseErr{s}
	}
	var a, b int
	if _, err := fmtSscanfInt(parts[0], &a); err != nil {
		return 0, err
	}
	if _, err := fmtSscanfInt(parts[1], &b); err != nil {
		return 0, err
	}
	*start = a
	*count = b
	return 0, nil
}

func fmtSscanfInt(s string, out *int) (int, error) {
	// Tiny self-contained int parser to avoid pulling fmt into this test file
	// (keeps the test tight and doesn't leak fmt scan state on failure).
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, &hunkParseErr{s}
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return 1, nil
}

// TestUnifiedDiff_IdenticalFiles just guards the early-return path.
func TestUnifiedDiff_IdenticalFiles(t *testing.T) {
	d := generateUnifiedDiff("a", "x\ny\n", "x\ny\n")
	if !strings.Contains(d, "identical") {
		t.Errorf("expected identical-file sentinel, got: %q", d)
	}
}
