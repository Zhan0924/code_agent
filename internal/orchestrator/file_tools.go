// Package orchestrator: file_tools.go adds autonomous file-system tools to the Agent.
// These tools let the LLM independently decide when to read, write, list, and test files
// within a workspace — enabling a full "think → act → observe → iterate" agentic loop.
package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/workspace"
	"go.uber.org/zap"
)

// defaultWorkspaceCmdTimeout 是 run_workspace_cmd 未配置 cmd_timeout 时的兜底
// 上限。从 2 分钟提升到 5 分钟,覆盖 LLM 生成的集成测试脚本(典型耗时
// 1-3 分钟,含 server 启停 + 多组 curl 探针)。LLM 可在 [0, 此值] 内通过
// tool args.timeout_seconds 提出更短上限。配置项 workspace.cmd_timeout
// 通过 main.go SetWorkspaceCmdTimeout 覆盖此值。
const defaultWorkspaceCmdTimeout = 5 * time.Minute

// allowedHostEnvVars is the closed allowlist of environment variables
// propagated into LLM-executed host commands. The previous implementation
// used cmd.Environ() and leaked every host variable — including AWS_*,
// GITHUB_TOKEN, Kube credentials, etc. — into whatever code the LLM chose to
// run. An allowlist is the only correct posture here: anything not on this
// list is inaccessible to the sandboxed command. Entries cover the standard
// dev-tool surface (PATH, HOME, toolchain caches, package registries, locale)
// and nothing else.
var allowedHostEnvVars = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL",
	"LANG", "LC_ALL", "LC_CTYPE", "TZ", "TERM",
	// Go toolchain
	"GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE", "GOPROXY", "GOSUMDB",
	"GOFLAGS", "GOTOOLCHAIN", "GOPRIVATE", "GONOSUMCHECK",
	// Node / npm
	"NODE_PATH", "NPM_CONFIG_REGISTRY", "NPM_CONFIG_CACHE",
	// Python
	"VIRTUAL_ENV", "PYTHONPATH", "PIP_INDEX_URL",
	// Rust
	"CARGO_HOME", "RUSTUP_HOME",
}

// minimalCommandEnv returns a pared-down environment containing only the
// host variables present in allowedHostEnvVars. Callers should extend the
// result with explicit KEY=VALUE additions they actually need.
func minimalCommandEnv() []string {
	env := make([]string, 0, len(allowedHostEnvVars))
	for _, k := range allowedHostEnvVars {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// bannedCommandPatterns matches dangerous shell patterns that should never
// be executed in a workspace context, regardless of the tool's stated purpose.
var bannedCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+(-[a-z]*f[a-z]*\s+)?/`),        // rm targeting root
	regexp.MustCompile(`(?i)\bmkfs\b`),                              // format filesystem
	regexp.MustCompile(`(?i)\bdd\s+if=`),                            // raw disk write
	regexp.MustCompile(`:\(\)\s*\{\s*:\|:\s*&\s*\}\s*;`),            // fork bomb
	regexp.MustCompile(`(?i)\bcurl\b.*\|\s*(ba)?sh`),                // pipe-to-shell
	regexp.MustCompile(`(?i)\bwget\b.*\|\s*(ba)?sh`),                // pipe-to-shell
	regexp.MustCompile(`(?i)\bnc\s+-[a-z]*l`),                       // netcat listen
	regexp.MustCompile(`(?i)\bchmod\s+[0-7]*s`),                     // setuid
	regexp.MustCompile(`(?i)\bchown\s+root\b`),                      // chown to root
	regexp.MustCompile(`(?i)\bsudo\b`),                              // privilege escalation
	regexp.MustCompile(`(?i)\bsu\s+`),                               // switch user
	regexp.MustCompile(`(?i)/etc/(passwd|shadow|sudoers)`),           // sensitive system files
	regexp.MustCompile(`(?i)\biptables\b`),                          // firewall manipulation
	regexp.MustCompile(`(?i)\bsystemctl\b`),                         // service management
	regexp.MustCompile(`(?i)\bshutdown\b`),                          // system shutdown
	regexp.MustCompile(`(?i)\breboot\b`),                            // system reboot
	regexp.MustCompile(`(?i)\bmount\b`),                             // filesystem mount
	regexp.MustCompile(`(?i)\bumount\b`),                            // filesystem unmount
}

// allowedCommandPrefixes defines the set of command prefixes considered safe
// for development workflows. Commands not starting with one of these are rejected.
var allowedCommandPrefixes = []string{
	"go ", "go\t", "python", "pip", "node", "npm", "npx", "pnpm",
	"yarn", "cargo", "rustc", "make", "cmake", "mvn", "gradle",
	"javac", "java ", "dotnet", "gcc", "g++", "clang",
	"cat ", "head ", "tail ", "grep ", "rg ", "find ", "fd ",
	"ls", "wc ", "sort ", "uniq ", "diff ", "file ",
	"echo ", "printf ", "test ", "true", "false",
	"mkdir ", "cp ", "mv ", "touch ", "rm ",
	"git ", "docker ", "kubectl ",
	"curl ", "wget ", "jq ", "yq ",
	"sed ", "awk ", "cut ", "tr ",
	"env ", "which ", "type ", "command ",
	"sh ", "bash ", "zsh ",
	"tsc", "eslint", "prettier", "jest", "vitest", "pytest",
	"golangci-lint", "staticcheck", "gopls",
	"ruff", "mypy", "black", "isort",
}

// validateWorkspaceCommand checks a command string for security violations.
// Returns an empty string if safe, or a rejection reason.
func validateWorkspaceCommand(command string) string {
	if strings.TrimSpace(command) == "" {
		return "empty command"
	}

	// Check banned patterns
	for _, pat := range bannedCommandPatterns {
		if pat.MatchString(command) {
			return fmt.Sprintf("matches banned pattern: %s", pat.String())
		}
	}

	// Extract the base command (first token, ignoring env var assignments)
	baseCmd := extractBaseCommand(command)
	if baseCmd == "" {
		return "could not determine base command"
	}

	// Check against allowed prefixes
	for _, prefix := range allowedCommandPrefixes {
		trimmed := strings.TrimSpace(prefix)
		if baseCmd == trimmed || strings.HasPrefix(baseCmd, trimmed) {
			return ""
		}
	}

	return fmt.Sprintf("command '%s' not in allowed list — use standard dev tools (go, python, node, make, git, etc.)", baseCmd)
}

// extractBaseCommand strips leading env assignments (KEY=val) and returns
// the actual command being invoked.
func extractBaseCommand(command string) string {
	parts := strings.Fields(command)
	for _, p := range parts {
		if strings.Contains(p, "=") && !strings.HasPrefix(p, "-") {
			continue
		}
		return p
	}
	return ""
}

// SetWorkspaceManager injects the workspace manager after construction,
// enabling autonomous file tools in the ReAct loop.
func (o *Orchestrator) SetWorkspaceManager(wm *workspace.Manager) {
	o.workspaceMgr = wm
	// [P0] Initialize precision edit engine and auto-test runner
	if wm != nil {
		o.editEngine = NewEditEngine(wm, o.logger)
		o.autoTestRunner = NewAutoTestRunner(wm, o.logger)
		// Register file and git tools into unified registry
		if o.toolRegistry != nil {
			if err := o.RegisterFileTools(o.toolRegistry); err != nil {
				o.logger.Error("failed to register file tools", zap.Error(err))
			}
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Tool Definitions (registered in RegisterFileTools)
// ═══════════════════════════════════════════════════════════════════════════════

func fileToolDefinitions() []models.ToolDefinition {
	return []models.ToolDefinition{
		{
			Name:        ToolReadFile,
			Description: "Read the contents of a file from the workspace. Use this to inspect code, configs, logs, or any text file before deciding on next actions.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID (or 'default' for the default workspace)"},
					"path": {"type": "string", "description": "Relative file path within the workspace, e.g. 'internal/models/user.go'"},
					"start_line": {"type": "integer", "description": "Optional: first line to read (1-based). Omit to read from the beginning."},
					"end_line": {"type": "integer", "description": "Optional: last line to read (1-based). Omit to read to the end."}
				},
				"required": ["path"]
			}`),
			Source:           "builtin",
			IsIdempotentRead: true,
		},
		{
			Name:        ToolWriteFile,
			Description: "Create or overwrite a file in the workspace. For files > 100 lines that already exist, PREFER patch_file instead to minimize token usage. Use write_file only for new files or complete rewrites.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID (or 'default')"},
					"path": {"type": "string", "description": "Relative file path to write, e.g. 'cmd/server/main.go'"},
					"content": {"type": "string", "description": "The full file content to write"}
				},
				"required": ["path", "content"]
			}`),
			Source:           "builtin",
			RiskLevel:        1, // Moderate risk: writes inside the isolated workspace, mirrors patch_file
			IsFileWrite:      true,
			TriggersAutoTest: true,
			InvalidatesCache: true,
		},
		{
			Name:        ToolPatchFile,
			Description: "Apply a targeted edit to an existing file by replacing a specific text section. Use this to fix bugs or modify specific functions without rewriting the entire file.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID (or 'default')"},
					"path": {"type": "string", "description": "Relative file path to patch"},
					"old_text": {"type": "string", "description": "The exact text to find and replace (must match exactly)"},
					"new_text": {"type": "string", "description": "The replacement text"}
				},
				"required": ["path", "old_text", "new_text"]
			}`),
			Source:           "builtin",
			RiskLevel:        1, // Moderate risk: modifies existing files
			IsFileWrite:      true,
			TriggersAutoTest: true,
			InvalidatesCache: true,
		},
		{
			Name:        ToolListFiles,
			Description: "List files and directories in a workspace path. Use this to understand project structure before reading or modifying files.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID (or 'default')"},
					"path": {"type": "string", "description": "Relative directory path to list (use '.' or '' for root)"},
					"recursive": {"type": "boolean", "description": "If true, list all files recursively as a tree view"}
				},
				"required": []
			}`),
			Source:           "builtin",
			IsIdempotentRead: true,
		},
		{
			Name:        ToolCreateDirectory,
			Description: "Create a directory (and any parent directories) in the workspace.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID (or 'default')"},
					"path": {"type": "string", "description": "Relative directory path to create, e.g. 'internal/handlers'"}
				},
				"required": ["path"]
			}`),
			Source: "builtin",
		},
		{
			Name:        ToolRunTests,
			Description: "Execute a test or build command in a Docker sandbox with the workspace files mounted. Use this to validate code changes, run unit tests, or check for compilation errors. The command runs inside the workspace directory. Returns stdout, stderr, and exit code.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID (or 'default')"},
					"language": {"type": "string", "enum": ["go", "python", "node", "bash"], "description": "Language runtime to use"},
					"command": {"type": "string", "description": "The test/build command to run, e.g. 'go test ./... -v' or 'python -m pytest tests/'"}
				},
				"required": ["language", "command"]
			}`),
			Source: "builtin",
		},
		{
			Name:        ToolRunWorkspaceCmd,
			Description: "Execute a shell command directly in the workspace directory WITHOUT Docker. This is the PREFERRED way to run 'go test', 'go build', 'go vet', 'python -m pytest', 'npm test', etc. It uses the host's installed toolchain directly, so it's fast and doesn't require pulling Docker images. Use this instead of run_tests for all compilation and test execution. Returns stdout, stderr, exit code, and duration. Default timeout is 5 minutes; long-running suites (integration tests with server startup/teardown) may need timeout_seconds set explicitly.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID (or 'default')"},
					"command": {"type": "string", "description": "Shell command to run in the workspace root, e.g. 'go test ./... -v -count=1', 'go build ./...', 'python -m pytest -v'"},
					"timeout_seconds": {"type": "integer", "minimum": 0, "description": "Optional max wall-clock seconds before the process group is SIGKILL'd. Capped server-side by workspace.cmd_timeout (default 5 min). Omit or set 0 to use the ceiling. Use this for known long suites (e.g. 300 for a multi-stage integration script)."}
				},
				"required": ["command"]
			}`),
			Source:    "builtin",
			RiskLevel: 1, // Moderate risk: host exec, gated by validateWorkspaceCommand + minimalCommandEnv (HITL threshold is >=2)
		},
		{
			Name:        ToolEditFile,
			Description: "Precision edit: replace a specific text section in a file. The old_text MUST match exactly once in the file (not 0, not 2+). After editing, the file is automatically lint/compile-checked — if the check fails, the edit is rolled back and you'll see the errors. This is SAFER than patch_file and should be preferred for all code modifications.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID (or 'default')"},
					"path": {"type": "string", "description": "Relative file path to edit"},
					"old_text": {"type": "string", "description": "The exact text to find — MUST appear exactly once in the file. Include enough context lines (function signature, comments) to make it unique."},
					"new_text": {"type": "string", "description": "The replacement text"}
				},
				"required": ["path", "old_text", "new_text"]
			}`),
			Source:           "builtin",
			RiskLevel:        1, // Moderate risk: modifies existing files with rollback
			IsFileWrite:      true,
			TriggersAutoTest: true,
			InvalidatesCache: true,
		},
		{
			Name:        ToolApplyDiff,
			Description: "Apply a unified diff patch to a file. The diff must be in standard unified diff format (output of 'diff -u'). After applying, the file is automatically lint/compile-checked — if the check fails, the edit is rolled back. Use this for large multi-line edits or when you have a diff from another source.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"workspace_id": {"type": "string", "description": "The workspace ID (or 'default')"},
					"path": {"type": "string", "description": "Relative file path to patch"},
					"diff": {"type": "string", "description": "Unified diff content (standard diff -u format with @@ hunks)"}
				},
				"required": ["path", "diff"]
			}`),
			Source:           "builtin",
			RiskLevel:        1, // Moderate risk: modifies existing files with rollback
			IsFileWrite:      true,
			TriggersAutoTest: true,
			InvalidatesCache: true,
		},
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Tool Execution Handlers
// ═══════════════════════════════════════════════════════════════════════════════

func (o *Orchestrator) toolReadFile(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
		StartLine   int    `json:"start_line"`
		EndLine     int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	if strings.TrimSpace(req.Path) == "" {
		return &models.ToolResult{
			Content: "Invalid arguments: 'path' must be a non-empty relative file path. Use list_files to discover available files first.",
			IsError: true,
		}, nil
	}

	ws := o.resolveWorkspaceCtx(ctx, req.WorkspaceID)
	if ws == nil {
		return &models.ToolResult{Content: "Workspace not found. Use list_files first to check available workspaces.", IsError: true}, nil
	}

	content, err := o.workspaceMgr.ReadFile(ws, req.Path)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("Failed to read '%s': %v", req.Path, err), IsError: true}, nil
	}

	// Apply line range if specified
	if req.StartLine > 0 || req.EndLine > 0 {
		lines := strings.Split(content, "\n")
		start := 0
		end := len(lines)
		if req.StartLine > 0 {
			start = req.StartLine - 1
		}
		if req.EndLine > 0 && req.EndLine < end {
			end = req.EndLine
		}
		if start >= len(lines) {
			start = len(lines) - 1
		}
		if start < 0 {
			start = 0
		}
		// Add line numbers for reference
		var numbered []string
		for i := start; i < end; i++ {
			numbered = append(numbered, fmt.Sprintf("%d | %s", i+1, lines[i]))
		}
		content = strings.Join(numbered, "\n")
	}

	// Truncate very large files
	if len(content) > 50000 {
		content = content[:50000] + "\n... [file truncated at 50000 chars, use start_line/end_line to read specific sections]"
	}

	o.logger.Debug("tool:read_file", zap.String("path", req.Path), zap.Int("bytes", len(content)))
	return &models.ToolResult{Content: content}, nil
}

func (o *Orchestrator) toolWriteFile(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	if strings.TrimSpace(req.Path) == "" {
		return &models.ToolResult{
			Content: "Invalid arguments: 'path' must be a non-empty relative file path (e.g. \"main.go\", \"pkg/util/helper.go\"). Use list_files to discover existing paths first.",
			IsError: true,
		}, nil
	}

	ws := o.resolveWorkspaceCtx(ctx, req.WorkspaceID)
	if ws == nil {
		return &models.ToolResult{Content: "Workspace not found", IsError: true}, nil
	}

	if err := o.workspaceMgr.WriteFile(ws, req.Path, req.Content); err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("Failed to write '%s': %v", req.Path, err), IsError: true}, nil
	}

	// [P2-7] Auto dependency management: run 'go mod tidy' after writing .go files
	hint := o.autoDepManagement(ctx, ws, req.Path)

	o.logger.Info("tool:write_file", zap.String("path", req.Path), zap.Int("bytes", len(req.Content)))
	msg := fmt.Sprintf("Successfully wrote %d bytes to %s", len(req.Content), req.Path)
	if hint != "" {
		msg += "\n" + hint
	}
	return &models.ToolResult{Content: msg}, nil
}

func (o *Orchestrator) toolPatchFile(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
		OldText     string `json:"old_text"`
		NewText     string `json:"new_text"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	if strings.TrimSpace(req.Path) == "" {
		return &models.ToolResult{
			Content: "Invalid arguments: 'path' must be a non-empty relative file path. Use list_files to find the target file first.",
			IsError: true,
		}, nil
	}

	ws := o.resolveWorkspaceCtx(ctx, req.WorkspaceID)
	if ws == nil {
		return &models.ToolResult{Content: "Workspace not found", IsError: true}, nil
	}

	// Read existing content
	existing, err := o.workspaceMgr.ReadFile(ws, req.Path)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("Failed to read '%s': %v", req.Path, err), IsError: true}, nil
	}

	// Check if old_text exists
	if !strings.Contains(existing, req.OldText) {
		return &models.ToolResult{
			Content: fmt.Sprintf("patch_file failed: old_text not found in '%s'. Read the file first to get the exact content.", req.Path),
			IsError: true,
		}, nil
	}

	// Apply patch (replace first occurrence)
	patched := strings.Replace(existing, req.OldText, req.NewText, 1)
	if err := o.workspaceMgr.WriteFile(ws, req.Path, patched); err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("Failed to write patched '%s': %v", req.Path, err), IsError: true}, nil
	}

	o.logger.Info("tool:patch_file", zap.String("path", req.Path),
		zap.Int("old_len", len(req.OldText)), zap.Int("new_len", len(req.NewText)))
	return &models.ToolResult{Content: fmt.Sprintf("Successfully patched %s (replaced %d chars with %d chars)", req.Path, len(req.OldText), len(req.NewText))}, nil
}

func (o *Orchestrator) toolListFiles(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
		Recursive   bool   `json:"recursive"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	ws := o.resolveWorkspaceCtx(ctx, req.WorkspaceID)
	if ws == nil {
		// If no workspace, list available workspaces
		list := o.workspaceMgr.ListWorkspaces()
		if len(list) == 0 {
			return &models.ToolResult{Content: "No workspaces available. The workspace will be created automatically when you use write_file."}, nil
		}
		var sb strings.Builder
		sb.WriteString("Available workspaces:\n")
		for _, w := range list {
			sb.WriteString(fmt.Sprintf("  - %s (%s)\n", w.ID, w.Project))
		}
		return &models.ToolResult{Content: sb.String()}, nil
	}

	if req.Recursive {
		tree := o.workspaceMgr.TreeString(ws)
		return &models.ToolResult{Content: tree}, nil
	}

	entries, err := o.workspaceMgr.ListDir(ws, req.Path)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("Failed to list '%s': %v", req.Path, err), IsError: true}, nil
	}

	return &models.ToolResult{Content: strings.Join(entries, "\n")}, nil
}

func (o *Orchestrator) toolCreateDirectory(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	ws := o.resolveWorkspaceCtx(ctx, req.WorkspaceID)
	if ws == nil {
		return &models.ToolResult{Content: "Workspace not found", IsError: true}, nil
	}

	if err := o.workspaceMgr.MkdirAll(ws, req.Path); err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("Failed to create directory '%s': %v", req.Path, err), IsError: true}, nil
	}

	return &models.ToolResult{Content: fmt.Sprintf("Directory '%s' created successfully", req.Path)}, nil
}

func (o *Orchestrator) toolRunTests(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Language    string `json:"language"`
		Command     string `json:"command"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	if o.sandboxMgr == nil {
		return &models.ToolResult{Content: "Sandbox not available (Docker not connected)", IsError: true}, nil
	}

	ws := o.resolveWorkspaceCtx(ctx, req.WorkspaceID)
	if ws == nil {
		return &models.ToolResult{Content: "Workspace not found. Create files first using write_file.", IsError: true}, nil
	}

	// Execute command inside container with workspace mounted
	cmd := fmt.Sprintf("cd /workspace && %s", req.Command)
	start := time.Now()
	result, err := o.sandboxMgr.ExecuteWithVolume(ctx, req.Language, cmd, ws.RootDir)
	if err != nil {
		return &models.ToolResult{Content: "Test execution failed: " + err.Error(), IsError: true}, nil
	}

	// Format output for LLM consumption
	var out strings.Builder
	fmt.Fprintf(&out, "Exit code: %d | Duration: %s\n", result.ExitCode, result.Duration)
	if result.Stdout != "" {
		fmt.Fprintf(&out, "--- OUTPUT ---\n%s\n", result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprintf(&out, "--- STDERR ---\n%s\n", result.Stderr)
	}

	if result.ExitCode == 0 {
		out.WriteString("✅ Tests/build PASSED\n")
	} else {
		out.WriteString("❌ Tests/build FAILED — review the errors above and fix with write_file or patch_file\n")
	}

	o.logger.Info("tool:run_tests",
		zap.String("command", req.Command),
		zap.Int("exit_code", result.ExitCode),
		zap.Duration("duration", time.Since(start)),
	)

	return &models.ToolResult{Content: out.String()}, nil
}

// toolRunWorkspaceCmd executes a shell command directly in the workspace directory
// using os/exec — no Docker required. This enables fast test execution using the
// host's toolchain (go, python, node, etc.).
func (o *Orchestrator) toolRunWorkspaceCmd(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		WorkspaceID    string `json:"workspace_id"`
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	ws := o.resolveWorkspaceCtx(ctx, req.WorkspaceID)
	if ws == nil {
		return &models.ToolResult{Content: "Workspace not found. Create files first using write_file.", IsError: true}, nil
	}

	// Security: comprehensive command validation
	if rejection := validateWorkspaceCommand(req.Command); rejection != "" {
		return &models.ToolResult{Content: "Command rejected: " + rejection, IsError: true}, nil
	}

	// 超时 = clamp(LLM 提议, [0, ceiling])。ceiling 来自配置 workspace.cmd_timeout
	// (main.go 注入,默认 defaultWorkspaceCmdTimeout)。LLM 不传或传 0 时按 ceiling
	// 兜底;传值过大被钳到 ceiling,避免一条命令把 ReAct loop 锁死半小时以上。
	ceiling := o.workspaceCmdTimeout
	if ceiling <= 0 {
		ceiling = defaultWorkspaceCmdTimeout
	}
	effectiveTimeout := ceiling
	if req.TimeoutSeconds > 0 {
		proposed := time.Duration(req.TimeoutSeconds) * time.Second
		if proposed < effectiveTimeout {
			effectiveTimeout = proposed
		}
	}
	cmdCtx, cancel := context.WithTimeout(ctx, effectiveTimeout)
	defer cancel()

	// Execute using sh -c for shell expansion support
	cmd := exec.CommandContext(cmdCtx, "sh", "-c", req.Command)
	cmd.Dir = ws.RootDir

	// Use process group so timeout kills ALL child processes (including backgrounded servers)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the entire process group (negative PID)
		pgid := cmd.Process.Pid
		return syscall.Kill(-pgid, syscall.SIGKILL)
	}

	// Capture stdout and stderr separately
	var stdout, stderr bytes.Buffer
	cmd.Stderr = &stderr

	// Scrub the environment: LLM-chosen commands must not see host secrets
	// (AWS_*, GITHUB_TOKEN, Kube credentials, etc.). Start from the allowlist,
	// then layer the minimum extras the toolchain needs.
	cmd.Env = minimalCommandEnv()

	progressCb := GetProgressCallback(ctx)

	var err error
	var duration time.Duration
	if progressCb != nil {
		// Stream stdout line-by-line through the progress callback
		stdoutPipe, pipeErr := cmd.StdoutPipe()
		if pipeErr != nil {
			return &models.ToolResult{Content: "Failed to create stdout pipe: " + pipeErr.Error(), IsError: true}, nil
		}
		start := time.Now()
		if startErr := cmd.Start(); startErr != nil {
			return &models.ToolResult{Content: "Failed to start command: " + startErr.Error(), IsError: true}, nil
		}
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text() + "\n"
			stdout.WriteString(line)
			progressCb(line)
		}
		err = cmd.Wait()
		duration = time.Since(start)
	} else {
		cmd.Stdout = &stdout
		start := time.Now()
		err = cmd.Run()
		duration = time.Since(start)
	}

	exitCode := 0
	if err != nil {
		if cmdCtx.Err() != nil {
			// Context timeout — killed entire process group
			exitCode = 137
			fmt.Fprintf(&stderr, "\n⚠️ Command timed out after %s and was killed (including all child processes). Pass a smaller timeout_seconds to retry faster, or split the command if it legitimately needs more time (server-side ceiling: %s).\n", effectiveTimeout, ceiling)
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &models.ToolResult{
				Content: fmt.Sprintf("Failed to execute command: %v", err),
				IsError: true,
			}, nil
		}
	}

	// Format output for LLM consumption
	var out strings.Builder
	fmt.Fprintf(&out, "Exit code: %d | Duration: %s\n", exitCode, duration.Round(time.Millisecond))

	stdoutStr := stdout.String()
	stderrStr := stderr.String()

	// [P0-3] Smart output handling: for test output, extract structured summary
	stdoutStr = smartTruncateOutput(stdoutStr, 30000)
	stderrStr = smartTruncateOutput(stderrStr, 15000)

	if stdoutStr != "" {
		fmt.Fprintf(&out, "--- STDOUT ---\n%s\n", stdoutStr)
	}
	if stderrStr != "" {
		fmt.Fprintf(&out, "--- STDERR ---\n%s\n", stderrStr)
	}

	if exitCode == 0 {
		out.WriteString("✅ Command SUCCEEDED\n")
	} else {
		out.WriteString("❌ Command FAILED — review errors above and fix with write_file or patch_file, then re-run\n")
	}

	o.logger.Info("tool:run_workspace_cmd",
		zap.String("command", req.Command),
		zap.String("workspace", ws.RootDir),
		zap.Int("exit_code", exitCode),
		zap.Duration("duration", duration),
	)

	return &models.ToolResult{Content: out.String()}, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// [P0-3] Smart Output Truncation
// ═══════════════════════════════════════════════════════════════════════════════

// smartTruncateOutput intelligently truncates command output while preserving
// the most useful parts: the HEAD (test names, setup) and TAIL (errors, summary).
// For Go test output, it also extracts a structured PASS/FAIL summary.
func smartTruncateOutput(output string, maxLen int) string {
	if len(output) <= maxLen {
		return output
	}

	// Try to extract a Go test summary if the output looks like test output
	if strings.Contains(output, "--- FAIL") || strings.Contains(output, "--- PASS") || strings.Contains(output, "FAIL\t") || strings.Contains(output, "ok  \t") {
		summary := extractTestSummary(output)
		if len(summary) > 0 && len(summary) < maxLen {
			return summary + "\n\n[Full output truncated from " + fmt.Sprintf("%d", len(output)) + " chars. Above is the extracted test summary.]"
		}
	}

	// Generic smart truncation: HEAD + TAIL
	headSize := maxLen / 3     // first third
	tailSize := maxLen * 2 / 3 // last two-thirds (errors usually at the end)
	if headSize+tailSize > maxLen {
		tailSize = maxLen - headSize
	}

	return output[:headSize] +
		"\n\n... [" + fmt.Sprintf("%d", len(output)-headSize-tailSize) + " chars omitted] ...\n\n" +
		output[len(output)-tailSize:]
}

// extractTestSummary parses Go test -v output and extracts a compact summary
// showing only PASS/FAIL lines and the final result.
func extractTestSummary(output string) string {
	lines := strings.Split(output, "\n")
	var summary []string
	var failures []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Capture test result lines
		if strings.HasPrefix(trimmed, "--- PASS") || strings.HasPrefix(trimmed, "--- FAIL") {
			summary = append(summary, trimmed)
		}
		// Capture package result lines
		if strings.HasPrefix(trimmed, "ok  \t") || strings.HasPrefix(trimmed, "FAIL\t") {
			summary = append(summary, trimmed)
		}
		// Capture failure details (indented lines after --- FAIL)
		if len(failures) > 0 || strings.HasPrefix(trimmed, "--- FAIL") {
			if strings.HasPrefix(trimmed, "--- FAIL") || (len(trimmed) > 0 && !strings.HasPrefix(trimmed, "---") && !strings.HasPrefix(trimmed, "ok") && !strings.HasPrefix(trimmed, "FAIL\t") && !strings.HasPrefix(trimmed, "=== RUN")) {
				failures = append(failures, line)
				if len(failures) > 30 { // cap failure details
					failures = append(failures, "... [more failure details truncated]")
					break
				}
			}
		}
	}

	if len(summary) == 0 {
		return ""
	}

	var result strings.Builder
	result.WriteString("=== TEST SUMMARY ===\n")
	for _, s := range summary {
		result.WriteString(s + "\n")
	}
	if len(failures) > 0 {
		result.WriteString("\n=== FAILURE DETAILS ===\n")
		for _, f := range failures {
			result.WriteString(f + "\n")
		}
	}
	return result.String()
}

// ═══════════════════════════════════════════════════════════════════════════════
// [P0] Precision Edit Tool Handler
// ═══════════════════════════════════════════════════════════════════════════════

// toolEditFile uses the EditEngine for safe, validated, lint-checked edits.
func (o *Orchestrator) toolEditFile(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
		OldText     string `json:"old_text"`
		NewText     string `json:"new_text"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	ws := o.resolveWorkspaceCtx(ctx, req.WorkspaceID)
	if ws == nil {
		return &models.ToolResult{Content: "Workspace not found", IsError: true}, nil
	}

	if o.editEngine == nil {
		// Fallback to legacy patch if edit engine not initialized
		return o.toolPatchFile(ctx, args)
	}

	result := o.editEngine.ApplyEdit(ctx, ws, EditOperation{
		Path:    req.Path,
		OldText: req.OldText,
		NewText: req.NewText,
	})

	o.logger.Info("tool:edit_file",
		zap.String("path", req.Path),
		zap.Bool("success", result.Success),
		zap.Bool("rolled_back", result.RolledBack),
		zap.Int("lint_errors", len(result.LintErrors)))

	return &models.ToolResult{
		Content: result.Message,
		IsError: !result.Success,
	}, nil
}

// toolApplyDiff applies a unified diff patch to a file using the EditEngine.
func (o *Orchestrator) toolApplyDiff(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Path        string `json:"path"`
		Diff        string `json:"diff"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	ws := o.resolveWorkspaceCtx(ctx, req.WorkspaceID)
	if ws == nil {
		return &models.ToolResult{Content: "Workspace not found", IsError: true}, nil
	}

	if o.editEngine == nil {
		return &models.ToolResult{Content: "Edit engine not available", IsError: true}, nil
	}

	result := o.editEngine.ApplyEdit(ctx, ws, EditOperation{
		Path:        req.Path,
		UnifiedDiff: req.Diff,
	})

	o.logger.Info("tool:apply_diff",
		zap.String("path", req.Path),
		zap.Bool("success", result.Success),
		zap.Bool("rolled_back", result.RolledBack),
		zap.Int("lint_errors", len(result.LintErrors)))

	return &models.ToolResult{
		Content: result.Message,
		IsError: !result.Success,
	}, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// [P0] Auto-Test After File Modification
// ═══════════════════════════════════════════════════════════════════════════════

// RunAutoTestAfterEdit runs tests related to recently edited files.
// Called from the reactLoop after edit_file, write_file, or patch_file.
func (o *Orchestrator) RunAutoTestAfterEdit(ctx context.Context, editedPaths []string) *TestResult {
	if o.autoTestRunner == nil {
		return nil
	}
	ws := o.resolveWorkspace("")
	if ws == nil {
		return nil
	}
	return o.autoTestRunner.AfterEdit(ctx, ws, editedPaths)
}

// ═══════════════════════════════════════════════════════════════════════════════
// [P2-7] Auto Dependency Management
// ═══════════════════════════════════════════════════════════════════════════════

// autoDepTimeout caps how long any single auto-dependency resolution may run.
// Before this cap, a network-starved `go mod tidy` / `npm install` / `pip
// install` could wedge the orchestrator's ReAct loop indefinitely.
const autoDepTimeout = 5 * time.Minute

// autoDepManagement runs dependency resolution commands after writing certain
// files. It honours the caller's context (cancelled by client disconnect or
// shutdown) and additionally caps each invocation at autoDepTimeout so a
// hung package index cannot block the whole ReAct turn. Returns a hint
// string to append to the tool result (empty if nothing done).
func (o *Orchestrator) autoDepManagement(ctx context.Context, ws *workspace.Workspace, path string) string {
	if ws == nil {
		return ""
	}

	cmdCtx, cancel := context.WithTimeout(ctx, autoDepTimeout)
	defer cancel()

	run := func(label, hint string, argv ...string) string {
		cmd := exec.CommandContext(cmdCtx, argv[0], argv[1:]...)
		cmd.Dir = ws.RootDir
		cmd.Env = minimalCommandEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			if cmdCtx.Err() != nil {
				o.logger.Warn("auto "+label+" timed out",
					zap.Duration("timeout", autoDepTimeout),
					zap.String("output", truncateForLog(string(out))))
				return fmt.Sprintf("⚠️ Auto '%s' timed out after %s", hint, autoDepTimeout)
			}
			o.logger.Warn("auto "+label+" failed",
				zap.Error(err),
				zap.String("output", truncateForLog(string(out))))
			return fmt.Sprintf("⚠️ Auto '%s' failed: %s", hint, strings.TrimSpace(string(out)))
		}
		return fmt.Sprintf("✅ Auto-ran '%s' successfully", hint)
	}

	switch {
	case strings.HasSuffix(path, ".go") || path == "go.mod" || path == "go.sum":
		if _, err := o.workspaceMgr.ReadFile(ws, "go.mod"); err != nil {
			return "" // no go.mod, skip
		}
		return run("go mod tidy", "go mod tidy", "go", "mod", "tidy")

	case path == "package.json":
		return run("npm install", "npm install", "npm", "install", "--no-audit", "--no-fund")

	case path == "requirements.txt":
		return run("pip install", "pip install -r requirements.txt",
			"pip", "install", "-q", "-r", "requirements.txt")
	}
	return ""
}

// truncateForLog keeps log entries tractable when a failing dep manager spills
// hundreds of lines of output.
func truncateForLog(s string) string {
	const cap = 2000
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "... [truncated]"
}

// ═══════════════════════════════════════════════════════════════════════════════
// [P1-5] Workspace Isolation
// ═══════════════════════════════════════════════════════════════════════════════

// ResolveSessionWorkspace creates or retrieves a per-session isolated workspace.
// Each session gets its own directory to prevent concurrent tasks from
// conflicting AND to enforce tenant isolation — a failure to create the
// session's own workspace used to fall back to an arbitrary workspace from
// ListWorkspaces()[0], which could return another tenant's workspace and
// leak files across users. We now return nil on failure; callers must treat
// nil as "no workspace available" and surface the error to the user rather
// than silently routing work into someone else's directory.
func (o *Orchestrator) ResolveSessionWorkspace(sessionID string) *workspace.Workspace {
	if o.workspaceMgr == nil {
		return nil
	}
	if sessionID == "" {
		// Empty session ID has no valid private workspace to map to. Refuse
		// rather than fall through to the default/first workspace and leak.
		o.logger.Warn("ResolveSessionWorkspace called with empty sessionID")
		return nil
	}
	if ws, ok := o.workspaceMgr.Get(sessionID); ok {
		return ws
	}
	// sessionID may be shorter than 8 chars in tests or short-UUID setups;
	// guard the slice to avoid panics.
	label := "session-" + sessionID
	if len(sessionID) > 8 {
		label = "session-" + sessionID[:8]
	}
	ws, err := o.workspaceMgr.Create(sessionID, label)
	if err != nil {
		o.logger.Error("failed to create session workspace — refusing to fall back to a shared workspace (would cross tenants)",
			zap.String("session_id", sessionID), zap.Error(err))
		return nil
	}
	o.logger.Info("created isolated workspace for session",
		zap.String("session_id", sessionID), zap.String("workspace_root", ws.RootDir))
	return ws
}

// ═══════════════════════════════════════════════════════════════════════════════
// Workspace Resolution
// ═══════════════════════════════════════════════════════════════════════════════

// resolveWorkspaceCtx is the session-aware entry point that tool handlers
// should use. When the LLM-supplied workspace_id is empty or the literal
// "default", we prefer the per-session isolated workspace (looked up by the
// sessionID stored in ctx) so files generated during a chat land in that
// session's workspace and the frontend "Open in Workspace" deep-link finds
// them. Without this, every tool call with no explicit workspace_id would
// drop files into the shared global "default" bucket, pooling outputs from
// every session into one directory.
//
// Falls back to the legacy resolveWorkspace (which manages the global
// "default") when no sessionID is in ctx (planner / background callers) or
// the session workspace cannot be materialised.
func (o *Orchestrator) resolveWorkspaceCtx(ctx context.Context, id string) *workspace.Workspace {
	if id != "" && id != "default" {
		return o.resolveWorkspace(id)
	}
	if sid, _ := ctx.Value(ctxKeySessionID).(string); sid != "" {
		if ws := o.ResolveSessionWorkspace(sid); ws != nil {
			return ws
		}
	}
	return o.resolveWorkspace(id)
}

// resolveWorkspace finds or creates a workspace for the given ID.
// If empty/"default", uses or creates the designated default workspace.
// IMPORTANT: never falls back to ListWorkspaces()[0] — that used to return
// an arbitrary existing workspace (possibly another tenant's) whenever no
// "default" existed, which was a cross-session data leak.
func (o *Orchestrator) resolveWorkspace(id string) *workspace.Workspace {
	if o.workspaceMgr == nil {
		return nil
	}

	if id == "" || id == "default" {
		// Look for an explicit default workspace.
		for _, ws := range o.workspaceMgr.ListWorkspaces() {
			if ws.Project == "default" {
				return ws
			}
		}
		// None exists — create a fresh one. Do NOT fall back to list[0].
		ws, err := o.workspaceMgr.Create("default", "default")
		if err != nil {
			o.logger.Error("failed to create default workspace", zap.Error(err))
			return nil
		}
		return ws
	}

	ws, _ := o.workspaceMgr.Get(id)
	return ws
}
