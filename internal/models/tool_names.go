package models

const (
	// File tools
	ToolReadFile        = "read_file"
	ToolWriteFile       = "write_file"
	ToolPatchFile       = "patch_file"
	ToolEditFile        = "edit_file"
	ToolApplyDiff       = "apply_diff"
	ToolListFiles       = "list_files"
	ToolCreateDirectory = "create_directory"
	ToolRunTests        = "run_tests"
	ToolRunWorkspaceCmd = "run_workspace_cmd"
	ToolDeleteFile      = "delete_file"

	// Execution tools
	ToolShellExec   = "shell_exec"
	ToolExecuteCode = "execute_code"

	// Git tools
	ToolGitStatus = "git_status"
	ToolGitDiff   = "git_diff"
	ToolGitCommit = "git_commit"
	ToolGitLog    = "git_log"
	ToolGitBranch = "git_branch"
	ToolGitShow   = "git_show"

	// RAG / Search tools
	ToolSearchCode = "search_code"

	// LSP tools
	ToolGotoDefinition  = "goto_definition"
	ToolFindReferences  = "find_references"
	ToolHoverInfo       = "hover_info"
	ToolRenameSymbol    = "rename_symbol"
	ToolDocumentSymbols = "document_symbols"
)
