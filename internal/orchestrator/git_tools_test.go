package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/workspace"
	"go.uber.org/zap"
)

// gitAvailable reports whether the `git` binary is usable.
// Tests that require git are skipped when it is missing.
func gitAvailable(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("git")
	return err == nil
}

// newGitTestOrchestrator returns a minimal Orchestrator sufficient to drive
// git tool calls (only uses logger + git binary on disk).
func newGitTestOrchestrator() *Orchestrator {
	return &Orchestrator{logger: zap.NewNop()}
}

// newGitTestWorkspace creates a temp workspace directory.
func newGitTestWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	dir := t.TempDir()
	return &workspace.Workspace{
		ID:        "ws-test",
		RootDir:   dir,
		Project:   "test-project",
		CreatedAt: time.Now(),
	}
}

// ── Tool Definitions ────────────────────────────────────────────────

func TestGitToolDefinitions_ContainsAllTools(t *testing.T) {
	defs := gitToolDefinitions()

	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
		if d.Source != "builtin" {
			t.Errorf("git tool %q source = %q, want 'builtin'", d.Name, d.Source)
		}
		// Validate the schema parses as JSON
		var schema map[string]any
		if err := json.Unmarshal(d.Parameters, &schema); err != nil {
			t.Errorf("git tool %q has invalid JSON schema: %v", d.Name, err)
		}
	}

	expected := []string{"git_status", "git_diff", "git_commit", "git_log", "git_branch"}
	for _, want := range expected {
		if !names[want] {
			t.Errorf("missing git tool %q in definitions (got %v)", want, names)
		}
	}
}

// ── ensureGitInit + runGit ──────────────────────────────────────────

func TestOrchestrator_EnsureGitInit_CreatesRepo(t *testing.T) {
	if !gitAvailable(t) {
		t.Skip("git binary not available")
	}
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)

	if err := o.ensureGitInit(ws); err != nil {
		t.Fatalf("ensureGitInit failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(ws.RootDir, ".git")); err != nil {
		t.Errorf(".git dir should exist after init: %v", err)
	}

	// Second call should be a no-op (repo already exists)
	if err := o.ensureGitInit(ws); err != nil {
		t.Errorf("second ensureGitInit should succeed (no-op), got %v", err)
	}
}

func TestOrchestrator_RunGit_ErrorOnInvalidCmd(t *testing.T) {
	if !gitAvailable(t) {
		t.Skip("git binary not available")
	}
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)

	_, err := o.runGit(ws, "this-is-not-a-valid-git-subcommand")
	if err == nil {
		t.Error("expected error for invalid git subcommand")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("error should mention git, got: %v", err)
	}
}

// ── toolGitStatus ───────────────────────────────────────────────────

func TestOrchestrator_ToolGitStatus(t *testing.T) {
	if !gitAvailable(t) {
		t.Skip("git binary not available")
	}
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)
	if err := o.ensureGitInit(ws); err != nil {
		t.Fatal(err)
	}

	// Create an untracked file
	if err := os.WriteFile(filepath.Join(ws.RootDir, "new.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := o.toolGitStatus(context.Background(), ws, nil)
	if err != nil {
		t.Fatalf("git_status failed: %v", err)
	}
	if !strings.Contains(out, "new.txt") {
		t.Errorf("status should mention new.txt, got: %q", out)
	}
}

// ── toolGitCommit (+ AutoCommitAfterEdit) ───────────────────────────

func TestOrchestrator_ToolGitCommit_FullCycle(t *testing.T) {
	if !gitAvailable(t) {
		t.Skip("git binary not available")
	}
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)

	// Write a file, then commit via tool
	if err := os.WriteFile(filepath.Join(ws.RootDir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	args := json.RawMessage(`{"message":"init"}`)
	out, err := o.toolGitCommit(context.Background(), ws, args)
	if err != nil {
		t.Fatalf("toolGitCommit failed: %v", err)
	}
	if !strings.Contains(out, "Committed successfully") {
		t.Errorf("output should mention commit success, got: %q", out)
	}

	// Verify commit exists via git log
	logOut, err := o.toolGitLog(context.Background(), ws, json.RawMessage(`{"count":5}`))
	if err != nil {
		t.Fatalf("toolGitLog failed: %v", err)
	}
	if !strings.Contains(logOut, "init") {
		t.Errorf("git_log should list 'init' commit, got: %q", logOut)
	}

	// Second commit with no changes → expect "Nothing to commit"
	out, err = o.toolGitCommit(context.Background(), ws, json.RawMessage(`{"message":"empty"}`))
	if err != nil {
		t.Fatalf("empty commit call failed: %v", err)
	}
	if !strings.Contains(out, "Nothing to commit") {
		t.Errorf("expected 'Nothing to commit' message, got: %q", out)
	}
}

func TestOrchestrator_ToolGitCommit_RequiresMessage(t *testing.T) {
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)

	_, err := o.toolGitCommit(context.Background(), ws, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error when message is empty")
	}
	if !strings.Contains(err.Error(), "message is required") {
		t.Errorf("error text unexpected: %v", err)
	}
}

func TestOrchestrator_ToolGitCommit_InvalidJSON(t *testing.T) {
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)
	_, err := o.toolGitCommit(context.Background(), ws, json.RawMessage(`{"message":`))
	if err == nil {
		t.Error("expected parse error for bad JSON")
	}
}

// ── toolGitDiff ─────────────────────────────────────────────────────

func TestOrchestrator_ToolGitDiff_UnstagedChange(t *testing.T) {
	if !gitAvailable(t) {
		t.Skip("git binary not available")
	}
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)
	if err := o.ensureGitInit(ws); err != nil {
		t.Fatal(err)
	}

	// Commit a base file
	file := filepath.Join(ws.RootDir, "a.txt")
	if err := os.WriteFile(file, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := o.toolGitCommit(context.Background(), ws, json.RawMessage(`{"message":"v1"}`)); err != nil {
		t.Fatal(err)
	}

	// Modify the file
	if err := os.WriteFile(file, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := o.toolGitDiff(context.Background(), ws, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("toolGitDiff failed: %v", err)
	}
	if !strings.Contains(out, "a.txt") {
		t.Errorf("diff should mention a.txt, got:\n%s", out)
	}
}

// ── toolGitBranch ───────────────────────────────────────────────────

func TestOrchestrator_ToolGitBranch_CreateAndCheckout(t *testing.T) {
	if !gitAvailable(t) {
		t.Skip("git binary not available")
	}
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)

	// Need an initial commit before branching
	if err := os.WriteFile(filepath.Join(ws.RootDir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := o.toolGitCommit(context.Background(), ws, json.RawMessage(`{"message":"init"}`)); err != nil {
		t.Fatal(err)
	}

	_, err := o.toolGitBranch(context.Background(), ws,
		json.RawMessage(`{"name":"feature/x","create":true}`))
	if err != nil {
		t.Fatalf("create branch failed: %v", err)
	}

	// Verify HEAD points to new branch
	status, _ := o.toolGitStatus(context.Background(), ws, nil)
	if !strings.Contains(status, "feature/x") {
		t.Errorf("status should show new branch, got: %q", status)
	}
}

func TestOrchestrator_ToolGitBranch_InvalidJSON(t *testing.T) {
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)
	_, err := o.toolGitBranch(context.Background(), ws, json.RawMessage(`{bad`))
	if err == nil {
		t.Error("expected JSON parse error")
	}
}

// ── AutoCommitAfterEdit ─────────────────────────────────────────────

func TestOrchestrator_AutoCommitAfterEdit(t *testing.T) {
	if !gitAvailable(t) {
		t.Skip("git binary not available")
	}
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)
	if err := o.ensureGitInit(ws); err != nil {
		t.Fatal(err)
	}

	// Initial commit to establish HEAD
	if err := os.WriteFile(filepath.Join(ws.RootDir, "base.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := o.toolGitCommit(context.Background(), ws, json.RawMessage(`{"message":"base"}`)); err != nil {
		t.Fatal(err)
	}

	// Modify a file and use AutoCommitAfterEdit
	if err := os.WriteFile(filepath.Join(ws.RootDir, "base.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.AutoCommitAfterEdit(ws, []string{"base.txt"}, "refactor base")

	// Verify the auto-commit appears in log
	out, err := o.toolGitLog(context.Background(), ws, json.RawMessage(`{"count":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "refactor base") {
		t.Errorf("expected auto-commit message in log, got:\n%s", out)
	}
}

func TestOrchestrator_AutoCommitAfterEdit_SkipsNoWorkspace(t *testing.T) {
	o := newGitTestOrchestrator()
	// Nil workspace and empty files → should return without panic
	o.AutoCommitAfterEdit(nil, []string{"x.go"}, "nop")
	o.AutoCommitAfterEdit(&workspace.Workspace{}, nil, "nop")
}

func TestOrchestrator_AutoCommitAfterEdit_SkipsNonRepo(t *testing.T) {
	if !gitAvailable(t) {
		t.Skip("git binary not available")
	}
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t) // NOT initialized as git repo

	if err := os.WriteFile(filepath.Join(ws.RootDir, "x.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Should NOT error even without git init
	o.AutoCommitAfterEdit(ws, []string{"x.go"}, "test")
}

// ── toolGitLog default count ────────────────────────────────────────

func TestOrchestrator_ToolGitLog_DefaultCount(t *testing.T) {
	if !gitAvailable(t) {
		t.Skip("git binary not available")
	}
	o := newGitTestOrchestrator()
	ws := newGitTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws.RootDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := o.toolGitCommit(context.Background(), ws, json.RawMessage(`{"message":"c1"}`)); err != nil {
		t.Fatal(err)
	}

	// No count field → should default to 10
	out, err := o.toolGitLog(context.Background(), ws, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("git_log default-count failed: %v", err)
	}
	if !strings.Contains(out, "c1") {
		t.Errorf("expected c1 in log output, got: %s", out)
	}
}
