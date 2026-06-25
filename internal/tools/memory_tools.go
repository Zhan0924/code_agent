package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/models"
)

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
		Description: "Append information to a specific section of the core memory.",
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
}

func (t *coreMemoryAppendTool) Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var parsed coreMemoryAppendArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	
	// Assuming a default userID and projectID for this implementation/test
	userID := "default_user"
	projectID := "default_project"

	err := t.coreManager.AppendToSection(ctx, userID, projectID, parsed.Section, parsed.Content)
	if err != nil {
		return &models.ToolResult{
			Content: fmt.Sprintf("Failed to append: %v", err),
			IsError: true,
		}, nil
	}

	return &models.ToolResult{
		Content: "Successfully appended to core memory section " + parsed.Section,
	}, nil
}

// coreMemoryReplaceTool
type coreMemoryReplaceTool struct {
	coreManager memory.CoreMemoryManager
}

func (t *coreMemoryReplaceTool) Definition() models.ToolDefinition {
	return models.ToolDefinition{
		Name:        "core_memory_replace",
		Description: "Replace specific content in a core memory section.",
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
}

func (t *coreMemoryReplaceTool) Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var parsed coreMemoryReplaceArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	userID := "default_user"
	projectID := "default_project"

	err := t.coreManager.ReplaceInSection(ctx, userID, projectID, parsed.Section, parsed.OldContent, parsed.NewContent)
	if err != nil {
		return &models.ToolResult{
			Content: fmt.Sprintf("Failed to replace: %v", err),
			IsError: true,
		}, nil
	}

	return &models.ToolResult{
		Content: "Successfully replaced content in core memory section " + parsed.Section,
	}, nil
}
