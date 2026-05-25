// git_tools.go adds Git integration tools to the Agent, enabling
// automatic commit, branch, diff, and log operations within workspaces.
package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/workspace"
	"go.uber.org/zap"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Git Tool Definitions
// ═══════════════════════════════════════════════════════════════════════════════

func gitToolDefinitions() []models.ToolDefinition {
	return []models.ToolDefinition{
		{
			Name:        "git_status",
			Description: "Show the working tree status of the workspace (staged, unstaged, untracked files).",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID"}
				},
				"required": []
			}`),
			Source: "builtin",
		},
		{
			Name:        "git_diff",
			Description: "Show changes between the working tree and the index (unstaged changes), or between commits.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID"},
					"staged": {"type": "boolean", "description": "If true, show staged changes (--cached)"},
					"path": {"type": "string", "description": "Optional: limit diff to a specific file path"}
				},
				"required": []
			}`),
			Source: "builtin",
		},
		{
			Name:        "git_commit",
			Description: "Stage all changes and create a git commit with the given message. Use this after making code changes to save progress.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID"},
					"message": {"type": "string", "description": "The commit message (should be descriptive)"},
					"paths": {"type": "array", "items": {"type": "string"}, "description": "Optional: specific files to stage. If empty, stages all changes."}
				},
				"required": ["message"]
			}`),
			Source:    "builtin",
			RiskLevel: 2, // High risk: mutates git history
		},
		{
			Name:        "git_log",
			Description: "Show recent git commit history.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID"},
					"count": {"type": "integer", "description": "Number of commits to show (default: 10)"}
				},
				"required": []
			}`),
			Source: "builtin",
		},
		{
			Name:        "git_branch",
			Description: "Create or switch to a git branch.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID"},
					"name": {"type": "string", "description": "Branch name to create or switch to"},
					"create": {"type": "boolean", "description": "If true, create a new branch"}
				},
				"required": ["name"]
			}`),
			Source: "builtin",
		},
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Git Tool Handlers
// ═══════════════════════════════════════════════════════════════════════════════

func (o *Orchestrator) toolGitStatus(_ context.Context, ws *workspace.Workspace, _ json.RawMessage) (string, error) {
	return o.runGit(ws, "status", "--porcelain", "-b")
}

func (o *Orchestrator) toolGitDiff(_ context.Context, ws *workspace.Workspace, args json.RawMessage) (string, error) {
	var params struct {
		Staged bool   `json:"staged"`
		Path   string `json:"path"`
	}
	_ = json.Unmarshal(args, &params)

	gitArgs := []string{"diff"}
	if params.Staged {
		gitArgs = append(gitArgs, "--cached")
	}
	gitArgs = append(gitArgs, "--stat")
	if params.Path != "" {
		gitArgs = append(gitArgs, "--", params.Path)
	}

	stat, err := o.runGit(ws, gitArgs...)
	if err != nil {
		return "", err
	}

	// Also get the actual diff (limited)
	diffArgs := []string{"diff"}
	if params.Staged {
		diffArgs = append(diffArgs, "--cached")
	}
	if params.Path != "" {
		diffArgs = append(diffArgs, "--", params.Path)
	}

	diff, err := o.runGit(ws, diffArgs...)
	if err != nil {
		return stat, nil // return stat even if full diff fails
	}

	// Truncate diff to 8K to avoid token explosion
	if len(diff) > 8192 {
		diff = diff[:8192] + "\n... (diff truncated, showing first 8KB)"
	}

	return fmt.Sprintf("Summary:\n%s\n\nDiff:\n%s", stat, diff), nil
}

func (o *Orchestrator) toolGitCommit(ctx context.Context, ws *workspace.Workspace, args json.RawMessage) (string, error) {
	var params struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid git_commit params: %w", err)
	}
	if params.Message == "" {
		return "", fmt.Errorf("commit message is required")
	}

	// Ensure git repo is initialized
	if err := o.ensureGitInit(ws); err != nil {
		return "", err
	}

	// Stage files
	if len(params.Paths) > 0 {
		addArgs := append([]string{"add"}, params.Paths...)
		if _, err := o.runGit(ws, addArgs...); err != nil {
			return "", fmt.Errorf("git add failed: %w", err)
		}
	} else {
		if _, err := o.runGit(ws, "add", "-A"); err != nil {
			return "", fmt.Errorf("git add -A failed: %w", err)
		}
	}

	// Check if there's anything to commit
	status, _ := o.runGit(ws, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		return "Nothing to commit, working tree clean.", nil
	}

	// Commit
	output, err := o.runGit(ws, "commit", "-m", params.Message)
	if err != nil {
		return "", fmt.Errorf("git commit failed: %w", err)
	}

	_ = ctx // context available for future async use
	return fmt.Sprintf("Committed successfully:\n%s", output), nil
}

func (o *Orchestrator) toolGitLog(_ context.Context, ws *workspace.Workspace, args json.RawMessage) (string, error) {
	var params struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(args, &params)
	if params.Count <= 0 {
		params.Count = 10
	}

	return o.runGit(ws, "log", fmt.Sprintf("--max-count=%d", params.Count), "--oneline", "--graph", "--decorate")
}

func (o *Orchestrator) toolGitBranch(_ context.Context, ws *workspace.Workspace, args json.RawMessage) (string, error) {
	var params struct {
		Name   string `json:"name"`
		Create bool   `json:"create"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid git_branch params: %w", err)
	}

	// Ensure git repo is initialized
	if err := o.ensureGitInit(ws); err != nil {
		return "", err
	}

	if params.Create {
		return o.runGit(ws, "checkout", "-b", params.Name)
	}
	return o.runGit(ws, "checkout", params.Name)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Git Helpers
// ═══════════════════════════════════════════════════════════════════════════════

// ensureGitInit initializes a git repo if not already initialized.
func (o *Orchestrator) ensureGitInit(ws *workspace.Workspace) error {
	// Check if .git exists
	_, err := o.runGit(ws, "rev-parse", "--git-dir")
	if err == nil {
		return nil // already initialized
	}

	// Initialize
	if _, err := o.runGit(ws, "init"); err != nil {
		return fmt.Errorf("git init failed: %w", err)
	}

	// Configure user for commits
	_, _ = o.runGit(ws, "config", "user.email", "agent@code-agent.local")
	_, _ = o.runGit(ws, "config", "user.name", "Code Agent")

	o.logger.Info("git repo initialized", zap.String("workspace", ws.ID))
	return nil
}

// runGit executes a git command in the workspace directory.
func (o *Orchestrator) runGit(ws *workspace.Workspace, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Disable git hooks to prevent malicious repositories from executing arbitrary code
	fullArgs := append([]string{"-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Dir = ws.RootDir

	// Minimal environment to prevent hooks from reading sensitive data
	cmd.Env = []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("git %s: %s (stderr: %s)", strings.Join(args, " "), err, stderr.String())
	}

	return stdout.String(), nil
}

// executeGitTool adapts the git handler signature (ctx, *Workspace, RawMessage) → (string, error)
// into the standard tool dispatch signature returning *models.ToolResult.
func (o *Orchestrator) executeGitTool(ctx context.Context, tc models.ToolCall) (*models.ToolResult, error) {
	var params struct {
		WorkspaceID string `json:"workspace_id"`
	}
	_ = json.Unmarshal(tc.Args, &params)

	ws := o.resolveWorkspace(params.WorkspaceID)
	if ws == nil {
		return &models.ToolResult{Content: "No workspace available for git operations", IsError: true}, nil
	}

	var output string
	var err error

	switch tc.Name {
	case "git_status":
		output, err = o.toolGitStatus(ctx, ws, tc.Args)
	case "git_diff":
		output, err = o.toolGitDiff(ctx, ws, tc.Args)
	case "git_commit":
		output, err = o.toolGitCommit(ctx, ws, tc.Args)
	case "git_log":
		output, err = o.toolGitLog(ctx, ws, tc.Args)
	case "git_branch":
		output, err = o.toolGitBranch(ctx, ws, tc.Args)
	default:
		return &models.ToolResult{Content: fmt.Sprintf("Unknown git tool: %s", tc.Name), IsError: true}, nil
	}

	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("Git error: %v", err), IsError: true}, nil
	}
	return &models.ToolResult{Content: output}, nil
}

// AutoCommitAfterEdit creates an automatic commit after successful code edits.
// Called by the ReAct loop after file modifications.
func (o *Orchestrator) AutoCommitAfterEdit(ws *workspace.Workspace, editedFiles []string, description string) {
	if ws == nil || len(editedFiles) == 0 {
		return
	}

	// Only auto-commit if git is initialized
	if _, err := o.runGit(ws, "rev-parse", "--git-dir"); err != nil {
		return // not a git repo, skip
	}

	// Stage only the edited files
	addArgs := append([]string{"add"}, editedFiles...)
	if _, err := o.runGit(ws, addArgs...); err != nil {
		o.logger.Debug("auto-commit: git add failed", zap.Error(err))
		return
	}

	// Check if there are staged changes
	status, _ := o.runGit(ws, "diff", "--cached", "--stat")
	if strings.TrimSpace(status) == "" {
		return
	}

	// Create commit
	msg := fmt.Sprintf("agent: %s", description)
	if len(msg) > 72 {
		msg = msg[:69] + "..."
	}

	if _, err := o.runGit(ws, "commit", "-m", msg); err != nil {
		o.logger.Debug("auto-commit failed", zap.Error(err))
	} else {
		o.logger.Info("auto-commit created",
			zap.String("workspace", ws.ID),
			zap.Strings("files", editedFiles),
			zap.String("message", msg),
		)
	}
}
