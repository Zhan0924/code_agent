// Package orchestrator — tool_names.go centralises the string identifiers of
// every built-in tool registered by the orchestrator. Prior to this file,
// these names were duplicated as bare string literals across many .go files
// in this package, so adding or renaming a tool meant a fragile codebase-wide
// grep. The constants below are the single source of truth for tool names
// used inside this package.
//
// Scope note: this PR only collapses orchestrator-package usage. Cross-package
// references (multiagent / agentloop / planner) still hold literals — those
// migrate in a follow-up to avoid introducing import cycles. The constants
// here intentionally cover only tools that have a registered ToolDefinition
// in this package; alias-only names referenced by speculative_cache (list_dir,
// rag_search, etc.) are forward-compat hints and stay as literals.
package orchestrator

const (
	// File tools (file_tools.go fileToolDefinitions + builtin_tools handler map)
	ToolReadFile        = "read_file"
	ToolWriteFile       = "write_file"
	ToolPatchFile       = "patch_file"
	ToolEditFile        = "edit_file"
	ToolApplyDiff       = "apply_diff"
	ToolListFiles       = "list_files"
	ToolCreateDirectory = "create_directory"
	ToolRunTests        = "run_tests"
	ToolRunWorkspaceCmd = "run_workspace_cmd"

	// Execution tools
	ToolShellExec   = "shell_exec"
	ToolExecuteCode = "execute_code"

	// Git tools (git_tools.go gitToolDefinitions + executeGitTool dispatch)
	ToolGitStatus = "git_status"
	ToolGitDiff   = "git_diff"
	ToolGitCommit = "git_commit"
	ToolGitLog    = "git_log"
	ToolGitBranch = "git_branch"

	// LSP tools
	ToolRenameSymbol = "rename_symbol"
)
