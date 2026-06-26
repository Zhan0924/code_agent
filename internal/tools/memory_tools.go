package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/models"
)

// defaultCoreMemoryUserID / defaultCoreMemoryProjectID 仅在 ctx 中没有任何
// 身份信号（典型场景：CLI 直接调工具、单元测试）时兜底。
//
// 历史上这两个常量是 "default_user" / "default_project" 硬编码，导致 *所有*
// 用户/项目共用同一份 CoreMemory（隐私 + 正确性双 bug）。现在改为：
//  1. 优先从 models.UserIDFromContext / ProjectIDFromContext 取真值；
//  2. 其次和 handleListMemory / session.AnonymousUserID 的归一化保持一致；
//  3. 永远不会再出现"两个用户写到同一个 key"。
const (
	defaultCoreMemoryUserID    = "anonymous"
	defaultCoreMemoryProjectID = "default"
)

// resolveMemoryIdentity 从 ctx 取出 (userID, projectID)；为空时归一化到
// 与 handleListMemory / handleMemoryStats 一致的默认值，避免 UI 看不到
// 自己刚 append 的内容（命名空间错位历史 bug）。
func resolveMemoryIdentity(ctx context.Context) (userID, projectID string) {
	userID = models.UserIDFromContext(ctx)
	if userID == "" {
		userID = defaultCoreMemoryUserID
	}
	projectID = models.ProjectIDFromContext(ctx)
	if projectID == "" {
		projectID = defaultCoreMemoryProjectID
	}
	return userID, projectID
}

// MemoryToolsProvider provides active memory management tools.
type MemoryToolsProvider struct {
	coreManager memory.CoreMemoryManager
}

// NewMemoryToolsProvider creates a new provider.
func NewMemoryToolsProvider(coreManager memory.CoreMemoryManager) *MemoryToolsProvider {
	return &MemoryToolsProvider{
		coreManager: coreManager,
	}
}

func (p *MemoryToolsProvider) Name() string {
	return "memory_tools"
}

func (p *MemoryToolsProvider) Tools() []Tool {
	return []Tool{
		&coreMemoryAppendTool{coreManager: p.coreManager},
		&coreMemoryReplaceTool{coreManager: p.coreManager},
	}
}

// coreMemoryAppendTool
type coreMemoryAppendTool struct {
	coreManager memory.CoreMemoryManager
}

func (t *coreMemoryAppendTool) Definition() models.ToolDefinition {
	return models.ToolDefinition{
		Name:        "core_memory_append",
		Description: "Append information to a specific section of the core memory. Scope 'project' (default) keeps the entry to the current project; 'user' makes it visible across all projects for this user (use for persistent personality/preferences).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"section": {
					"type": "string",
					"enum": ["persona", "human_context", "project_context"],
					"description": "The section to append to."
				},
				"content": {
					"type": "string",
					"description": "The content to append."
				},
				"scope": {
					"type": "string",
					"enum": ["project", "user"],
					"description": "Storage scope. Default 'project'."
				}
			},
			"required": ["section", "content"]
		}`),
		Source: "builtin",
	}
}

type coreMemoryAppendArgs struct {
	Section string `json:"section"`
	Content string `json:"content"`
	Scope   string `json:"scope,omitempty"`
}

// parseScope returns the requested CoreMemoryScope. Empty/unknown values
// default to project — a safer default than silently leaking writes into
// user-global state.
func parseScope(raw string) memory.CoreMemoryScope {
	switch memory.CoreMemoryScope(raw) {
	case memory.CoreScopeUser:
		return memory.CoreScopeUser
	default:
		return memory.CoreScopeProject
	}
}

func (t *coreMemoryAppendTool) Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var parsed coreMemoryAppendArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	userID, projectID := resolveMemoryIdentity(ctx)
	scope := parseScope(parsed.Scope)
	err := t.coreManager.AppendToSectionScoped(ctx, userID, projectID, scope, parsed.Section, parsed.Content)
	if err != nil {
		return &models.ToolResult{
			Content: fmt.Sprintf("Failed to append: %v", err),
			IsError: true,
		}, nil
	}

	return &models.ToolResult{
		Content: fmt.Sprintf("Successfully appended to core memory section %s (scope=%s)", parsed.Section, scope),
	}, nil
}

// coreMemoryReplaceTool
type coreMemoryReplaceTool struct {
	coreManager memory.CoreMemoryManager
}

func (t *coreMemoryReplaceTool) Definition() models.ToolDefinition {
	return models.ToolDefinition{
		Name:        "core_memory_replace",
		Description: "Replace specific content in a core memory section. Scope must match the scope the section was written to (default 'project').",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"section": {
					"type": "string",
					"enum": ["persona", "human_context", "project_context"],
					"description": "The section to modify."
				},
				"old_content": {
					"type": "string",
					"description": "The exact old string to be replaced."
				},
				"new_content": {
					"type": "string",
					"description": "The new string."
				},
				"scope": {
					"type": "string",
					"enum": ["project", "user"],
					"description": "Storage scope. Default 'project'."
				}
			},
			"required": ["section", "old_content", "new_content"]
		}`),
		Source: "builtin",
	}
}

type coreMemoryReplaceArgs struct {
	Section    string `json:"section"`
	OldContent string `json:"old_content"`
	NewContent string `json:"new_content"`
	Scope      string `json:"scope,omitempty"`
}

func (t *coreMemoryReplaceTool) Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var parsed coreMemoryReplaceArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	userID, projectID := resolveMemoryIdentity(ctx)
	scope := parseScope(parsed.Scope)
	err := t.coreManager.ReplaceInSectionScoped(ctx, userID, projectID, scope, parsed.Section, parsed.OldContent, parsed.NewContent)
	if err != nil {
		return &models.ToolResult{
			Content: fmt.Sprintf("Failed to replace: %v", err),
			IsError: true,
		}, nil
	}

	return &models.ToolResult{
		Content: fmt.Sprintf("Successfully replaced content in core memory section %s (scope=%s)", parsed.Section, scope),
	}, nil
}
