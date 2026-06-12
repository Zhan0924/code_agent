package orchestrator

import "github.com/agent/code_agent/internal/models"

const (
	// File tools (file_tools.go fileToolDefinitions + builtin_tools handler map)
	ToolReadFile        = models.ToolReadFile
	ToolWriteFile       = models.ToolWriteFile
	ToolPatchFile       = models.ToolPatchFile
	ToolEditFile        = models.ToolEditFile
	ToolApplyDiff       = models.ToolApplyDiff
	ToolListFiles       = models.ToolListFiles
	ToolCreateDirectory = models.ToolCreateDirectory
	ToolRunTests        = models.ToolRunTests
	ToolRunWorkspaceCmd = models.ToolRunWorkspaceCmd

	// Execution tools
	ToolShellExec   = models.ToolShellExec
	ToolExecuteCode = models.ToolExecuteCode

	// Git tools (git_tools.go gitToolDefinitions + executeGitTool dispatch)
	ToolGitStatus = models.ToolGitStatus
	ToolGitDiff   = models.ToolGitDiff
	ToolGitCommit = models.ToolGitCommit
	ToolGitLog    = models.ToolGitLog
	ToolGitBranch = models.ToolGitBranch

	// LSP tools
	ToolRenameSymbol = models.ToolRenameSymbol
)
