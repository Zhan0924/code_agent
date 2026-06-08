package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/tools"
	"go.uber.org/zap"
)

// RegisterPTYTools registers persistent shell execution tools when PTY manager is available.
func (o *Orchestrator) RegisterPTYTools(reg *tools.Registry) error {
	if o.ptyManager == nil {
		return nil
	}

	tool := &builtinTool{
		def: models.ToolDefinition{
			Name:        ToolShellExec,
			Description: "Execute a command in a persistent shell session. State (cwd, env vars, aliases) persists across calls within the same workspace.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {
						"type": "string",
						"description": "Shell command to execute"
					},
					"timeout": {
						"type": "integer",
						"description": "Timeout in seconds (default: 120)",
						"default": 120
					}
				},
				"required": ["command"]
			}`),
			Source:    "builtin",
			RiskLevel: 1,
		},
		handler: o.toolShellExec,
	}

	return reg.Register(tool)
}

func (o *Orchestrator) toolShellExec(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var params struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	if params.Timeout == 0 {
		params.Timeout = 120
	}

	// Security: validate command using existing patterns。
	// PTY 路径只关心 reject;warning(`| head`/`| tail`)忽略 —— PTY 是交互式
	// session,组合管道少见,且无独立工具结果末尾可追加。
	if rejection, _ := validateWorkspaceCommand(params.Command); rejection != "" {
		return &models.ToolResult{Content: "Command rejected: " + rejection, IsError: true}, nil
	}

	// Get current workspace ID from context or use default
	workspaceID := o.getCurrentWorkspaceID(ctx)
	if workspaceID == "" {
		return &models.ToolResult{Content: "No active workspace. Create files first using write_file.", IsError: true}, nil
	}

	sess, err := o.ptyManager.GetOrCreate(ctx, workspaceID)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("Failed to get PTY session: %v", err), IsError: true}, nil
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(params.Timeout)*time.Second)
	defer cancel()

	result, err := sess.Execute(execCtx, params.Command)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("Execution error: %v", err), IsError: true}, nil
	}

	content := result.Output
	if result.ExitCode != 0 {
		content = fmt.Sprintf("[exit code: %d]\n%s", result.ExitCode, content)
	}
	if result.Truncated {
		content += "\n... (output truncated)"
	}

	o.logger.Info("tool:shell_exec",
		zap.String("command", params.Command),
		zap.Int("exit_code", result.ExitCode),
		zap.Duration("duration", result.Duration),
		zap.Bool("truncated", result.Truncated))

	return &models.ToolResult{
		Content: content,
		IsError: result.ExitCode != 0,
	}, nil
}

// getCurrentWorkspaceID extracts workspace ID from context or returns a default.
func (o *Orchestrator) getCurrentWorkspaceID(ctx context.Context) string {
	// Try to get from workspace manager's current workspace
	if o.workspaceMgr != nil {
		// Workspace manager doesn't expose current workspace directly,
		// so we use a heuristic: if there's any workspace, use the first one
		// In practice, the workspace ID should be passed via context or session
		// For now, return a default that will be created on demand
		return "default"
	}
	return "default"
}
