// builtin_tools.go wraps orchestrator's built-in tool handlers as tools.Tool
// implementations, enabling unified dispatch through tools.Registry.
package orchestrator

import (
	"context"
	"encoding/json"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/tools"
)

// builtinTool adapts an orchestrator method into the tools.Tool interface.
type builtinTool struct {
	def     models.ToolDefinition
	handler func(context.Context, json.RawMessage) (*models.ToolResult, error)
}

func (t *builtinTool) Definition() models.ToolDefinition {
	return t.def
}

func (t *builtinTool) Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	return t.handler(ctx, args)
}

// RegisterBuiltinTools registers all built-in tools (execute_code, search_code)
// into the given registry. File and git tools are registered separately when
// workspace manager is available.
func (o *Orchestrator) RegisterBuiltinTools(reg *tools.Registry) error {
	builtins := []struct {
		name        string
		description string
		params      json.RawMessage
		handler     func(context.Context, json.RawMessage) (*models.ToolResult, error)
	}{
		{
			name:        "execute_code",
			description: "Execute code in a sandboxed Docker container. Returns stdout, stderr, and exit code.",
			params:      json.RawMessage(`{"type":"object","properties":{"language":{"type":"string","enum":["python","go","bash","node"],"description":"Programming language"},"code":{"type":"string","description":"Code to execute"}},"required":["language","code"]}`),
			handler:     o.toolExecuteCode,
		},
		{
			name:        "search_code",
			description: "Search the indexed codebase using semantic and keyword search. Returns matching code snippets with file paths and line numbers.",
			params:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Natural language search query or exact symbol name"}},"required":["query"]}`),
			handler:     o.toolSearchCode,
		},
	}

	for _, b := range builtins {
		tool := &builtinTool{
			def: models.ToolDefinition{
				Name:        b.name,
				Description: b.description,
				Parameters:  b.params,
				Source:      "builtin",
			},
			handler: b.handler,
		}
		if err := reg.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

// RegisterFileTools registers file and git tools when workspace manager is available.
func (o *Orchestrator) RegisterFileTools(reg *tools.Registry) error {
	if o.workspaceMgr == nil {
		return nil
	}

	// File tools
	fileDefs := fileToolDefinitions()
	fileHandlers := map[string]func(context.Context, json.RawMessage) (*models.ToolResult, error){
		"read_file":         o.toolReadFile,
		"write_file":        o.toolWriteFile,
		"patch_file":        o.toolPatchFile,
		"edit_file":         o.toolEditFile,
		"apply_diff":        o.toolApplyDiff,
		"list_files":        o.toolListFiles,
		"create_directory":  o.toolCreateDirectory,
		"run_tests":         o.toolRunTests,
		"run_workspace_cmd": o.toolRunWorkspaceCmd,
	}

	for _, def := range fileDefs {
		handler, ok := fileHandlers[def.Name]
		if !ok {
			continue
		}
		tool := &builtinTool{def: def, handler: handler}
		if err := reg.Register(tool); err != nil {
			return err
		}
	}

	// Git tools - need special wrapper since they use executeGitTool
	gitDefs := gitToolDefinitions()
	for _, def := range gitDefs {
		gitDef := def // capture for closure
		tool := &builtinTool{
			def: gitDef,
			handler: func(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
				tc := models.ToolCall{Name: gitDef.Name, Args: args}
				return o.executeGitTool(ctx, tc)
			},
		}
		if err := reg.Register(tool); err != nil {
			return err
		}
	}

	return nil
}
