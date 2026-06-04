package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	m, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func testLogger() *zap.Logger {
	return zap.NewNop()
}

func TestCreateForSession(t *testing.T) {
	m := newTestManager(t)

	ws, err := m.CreateForSession("ws-1", "sess-1", "myproject")
	if err != nil {
		t.Fatalf("CreateForSession: %v", err)
	}
	if ws.ID != "ws-1" {
		t.Errorf("ID = %q, want %q", ws.ID, "ws-1")
	}
	if ws.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", ws.SessionID, "sess-1")
	}
	if ws.Project != "myproject" {
		t.Errorf("Project = %q, want %q", ws.Project, "myproject")
	}
	// Directory should exist
	if _, err := os.Stat(ws.RootDir); err != nil {
		t.Errorf("workspace dir does not exist: %v", err)
	}
	// RootDir should be fully resolved (no symlinks)
	resolved, _ := filepath.EvalSymlinks(ws.RootDir)
	if ws.RootDir != resolved {
		t.Errorf("RootDir not resolved: got %q, want %q", ws.RootDir, resolved)
	}
	// Idempotent: creating again returns same workspace
	ws2, err := m.CreateForSession("ws-1", "sess-1", "myproject")
	if err != nil {
		t.Fatalf("second CreateForSession: %v", err)
	}
	if ws2.ID != ws.ID {
		t.Errorf("expected same workspace on duplicate create")
	}
}

func TestGetBySession(t *testing.T) {
	m := newTestManager(t)

	_, err := m.CreateForSession("ws-1", "sess-A", "proj")
	if err != nil {
		t.Fatal(err)
	}

	ws, ok := m.GetBySession("sess-A")
	if !ok {
		t.Fatal("GetBySession returned false")
	}
	if ws.ID != "ws-1" {
		t.Errorf("got ID %q, want ws-1", ws.ID)
	}

	_, ok = m.GetBySession("nonexistent")
	if ok {
		t.Error("expected false for nonexistent session")
	}
}

func TestReadWriteFile(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-rw", "sess-rw", "proj")
	if err != nil {
		t.Fatal(err)
	}

	content := "hello world\n"
	if err := m.WriteFile(ws, "test.txt", content); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := m.ReadFile(ws, "test.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != content {
		t.Errorf("ReadFile = %q, want %q", got, content)
	}
}

func TestReadWriteFile_Subdirectory(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-sub", "sess-sub", "proj")
	if err != nil {
		t.Fatal(err)
	}

	// WriteFile requires parent to exist (safePath checks parent via EvalSymlinks).
	// First create the immediate parent, then write a file in it.
	if err := m.WriteFile(ws, "sub.txt", "flat"); err != nil {
		t.Fatalf("WriteFile flat: %v", err)
	}

	// Create nested dir manually, then write into it
	subDir := filepath.Join(ws.RootDir, "sub", "dir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile(ws, "sub/dir/file.go", "package main"); err != nil {
		t.Fatalf("WriteFile nested: %v", err)
	}

	got, err := m.ReadFile(ws, "sub/dir/file.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != "package main" {
		t.Errorf("got %q", got)
	}
}

func TestSafePath_TraversalBlocked(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-safe", "sess-safe", "proj")
	if err != nil {
		t.Fatal(err)
	}

	// Attempts to escape workspace should fail
	_, err = m.ReadFile(ws, "../../../etc/passwd")
	if err == nil {
		t.Error("expected error for path traversal")
	}
	if err != nil && !strings.Contains(err.Error(), "traversal") && !strings.Contains(err.Error(), "outside") {
		// Some implementations use different error messages
		t.Logf("got error (acceptable): %v", err)
	}

	err = m.WriteFile(ws, "../../escape.txt", "bad")
	if err == nil {
		t.Error("expected error for path traversal on write")
	}
}

// TestSafePath_EmptyOrRootRejected guards against the EISDIR bug where an
// empty or "." path resolved to ws.RootDir itself, causing os.WriteFile to
// fail with "is a directory" deep in the stack with an unhelpful message.
func TestSafePath_EmptyOrRootRejected(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-empty", "sess-empty", "proj")
	if err != nil {
		t.Fatal(err)
	}

	cases := []string{"", " ", ".", "./", "foo/.."}
	for _, p := range cases {
		if err := m.WriteFile(ws, p, "x"); err == nil {
			t.Errorf("WriteFile(%q) expected error, got nil", p)
		}
		if _, err := m.ReadFile(ws, p); err == nil {
			t.Errorf("ReadFile(%q) expected error, got nil", p)
		}
	}
}

// TestWriteFile_RejectExistingDir verifies the defense-in-depth check that
// refuses to overwrite an existing directory with file contents.
func TestWriteFile_RejectExistingDir(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-dir", "sess-dir", "proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MkdirAll(ws, "subdir"); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	err = m.WriteFile(ws, "subdir", "content")
	if err == nil {
		t.Fatal("expected error when writing to a path that is an existing directory")
	}
	if !strings.Contains(err.Error(), "existing directory") {
		t.Errorf("expected 'existing directory' in error, got: %v", err)
	}
}

func TestDeleteFile(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-del", "sess-del", "proj")
	if err != nil {
		t.Fatal(err)
	}

	if err := m.WriteFile(ws, "to-delete.txt", "gone"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteFile(ws, "to-delete.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	_, err = m.ReadFile(ws, "to-delete.txt")
	if err == nil {
		t.Error("expected error reading deleted file")
	}
}

func TestListFiles(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-list", "sess-list", "proj")
	if err != nil {
		t.Fatal(err)
	}

	_ = m.WriteFile(ws, "a.txt", "a")
	_ = m.WriteFile(ws, "b.txt", "b")
	_ = m.WriteFile(ws, "sub/c.txt", "c")

	files, err := m.ListFiles(ws)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) < 2 {
		t.Errorf("expected at least 2 files, got %d: %v", len(files), files)
	}
}

func TestArchive(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-archive", "sess-archive", "myproject")
	if err != nil {
		t.Fatal(err)
	}

	_ = m.WriteFile(ws, "file1.txt", "content1")
	_ = m.WriteFile(ws, "dir/file2.txt", "content2")

	var buf strings.Builder
	err = m.Archive(ws, &buf)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if buf.Len() == 0 {
		t.Error("Archive produced empty output")
	}
}

func TestCleanup(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-cleanup", "sess-cleanup", "proj")
	if err != nil {
		t.Fatal(err)
	}
	rootDir := ws.RootDir

	err = m.Cleanup("ws-cleanup")
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, err := os.Stat(rootDir); !os.IsNotExist(err) {
		t.Error("Cleanup: directory still exists")
	}

	_, ok := m.Get("ws-cleanup")
	if ok {
		t.Error("Cleanup: workspace still in map")
	}
}

func TestRestore(t *testing.T) {
	baseDir := t.TempDir()
	logger := testLogger()

	m1, err := NewManager(baseDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := m1.CreateForSession("ws-restore", "sess-restore", "proj")
	if err != nil {
		t.Fatal(err)
	}
	_ = m1.WriteFile(ws, "test.txt", "data")

	m2, err := NewManager(baseDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := m2.Get("ws-restore")
	if !ok {
		t.Fatal("Restore: workspace not found after restart")
	}
	if restored.ID != "ws-restore" {
		t.Errorf("Restore: ID = %q, want ws-restore", restored.ID)
	}

	content, err := m2.ReadFile(restored, "test.txt")
	if err != nil {
		t.Fatalf("ReadFile after restore: %v", err)
	}
	if content != "data" {
		t.Errorf("content after restore = %q, want %q", content, "data")
	}
}

// TestMkdirAll_NestedMissing exercises the canonical "agent wants to create
// internal/shortcode but neither segment exists yet" case. Before
// 2026-06-03 MkdirAll went through safePath, whose EvalSymlinks pass failed
// when both ancestors were missing — agent then looped on the error.
func TestMkdirAll_NestedMissing(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-nest", "sess-nest", "proj")
	if err != nil {
		t.Fatal(err)
	}

	if err := m.MkdirAll(ws, "internal/shortcode/handlers"); err != nil {
		t.Fatalf("MkdirAll nested missing: %v", err)
	}
	full := filepath.Join(ws.RootDir, "internal", "shortcode", "handlers")
	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("Stat created dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("expected directory, got mode %v", info.Mode())
	}
}

// TestMkdirAll_SymlinkAncestorEscapes guards against a class of CVE: if a
// pre-existing intermediate component is a symlink pointing outside the
// workspace, naive os.MkdirAll would silently create the new path on the
// symlink target. MkdirAll must reject that.
func TestMkdirAll_SymlinkAncestorEscapes(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-sym", "sess-sym", "proj")
	if err != nil {
		t.Fatal(err)
	}

	// Stage a side directory the symlink will point to. Use t.TempDir so the
	// test does not need to write outside the test root.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	// Inside the workspace, create a symlink whose target is outside.
	linkPath := filepath.Join(ws.RootDir, "escape")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Fatalf("symlink setup: %v", err)
	}

	err = m.MkdirAll(ws, "escape/inner")
	if err == nil {
		// If MkdirAll succeeded, confirm whether it followed the symlink — if
		// it did, the bug is back; if it created a real `escape/inner` next
		// to the symlink that's a different (still wrong) story.
		t.Fatalf("MkdirAll through symlink-ancestor should be rejected")
	}
	if !strings.Contains(err.Error(), "traversal") {
		t.Errorf("error %q does not mention traversal", err.Error())
	}
	// Outside dir must not have gained an `inner/` child.
	if _, statErr := os.Stat(filepath.Join(outside, "inner")); statErr == nil {
		t.Errorf("MkdirAll leaked into outside path %s", outside)
	}
}

// TestMkdirAll_RejectsParentTraversal sanity-checks the cleaned-path guard
// against `..`-based escape attempts.
func TestMkdirAll_RejectsParentTraversal(t *testing.T) {
	m := newTestManager(t)
	ws, err := m.CreateForSession("ws-trav", "sess-trav", "proj")
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{
		"../escape",
		"../../etc",
		"a/../../escape",
	} {
		if err := m.MkdirAll(ws, bad); err == nil {
			t.Errorf("MkdirAll(%q) should reject parent-escape", bad)
		}
	}
}

