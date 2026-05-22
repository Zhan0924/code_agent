package multiagent

import (
	"context"
	"fmt"

	"github.com/agent/code_agent/internal/models"
)

// FilteredToolExecutor wraps a ToolExecutor and only allows execution of
// tools in the allowlist.
type FilteredToolExecutor struct {
	inner   ToolExecutorInterface
	allowed map[string]bool
}

// ToolExecutorInterface matches agentloop.ToolExecutor.
type ToolExecutorInterface interface {
	Execute(ctx context.Context, tc models.ToolCall) (*models.ToolResult, error)
}

// NewFilteredToolExecutor creates a tool executor that rejects disallowed tools.
func NewFilteredToolExecutor(inner ToolExecutorInterface, allowedTools []string) *FilteredToolExecutor {
	allowed := make(map[string]bool, len(allowedTools))
	for _, t := range allowedTools {
		allowed[t] = true
	}
	return &FilteredToolExecutor{inner: inner, allowed: allowed}
}

func (f *FilteredToolExecutor) Execute(ctx context.Context, tc models.ToolCall) (*models.ToolResult, error) {
	if !f.allowed[tc.Name] {
		return &models.ToolResult{
			ToolCallID: tc.ID,
			Content:    fmt.Sprintf("tool %q is not allowed for this sub-agent", tc.Name),
			IsError:    true,
		}, nil
	}
	return f.inner.Execute(ctx, tc)
}

// FilteredToolProvider wraps a ToolProvider and only exposes tools in the allowlist.
type FilteredToolProvider struct {
	inner   ToolProviderInterface
	allowed map[string]bool
}

// ToolProviderInterface matches agentloop.ToolProvider.
type ToolProviderInterface interface {
	Definitions() []models.ToolDefinition
}

// NewFilteredToolProvider creates a tool provider that only exposes allowed tools.
func NewFilteredToolProvider(inner ToolProviderInterface, allowedTools []string) *FilteredToolProvider {
	allowed := make(map[string]bool, len(allowedTools))
	for _, t := range allowedTools {
		allowed[t] = true
	}
	return &FilteredToolProvider{inner: inner, allowed: allowed}
}

func (f *FilteredToolProvider) Definitions() []models.ToolDefinition {
	all := f.inner.Definitions()
	filtered := make([]models.ToolDefinition, 0, len(f.allowed))
	for _, td := range all {
		if f.allowed[td.Name] {
			filtered = append(filtered, td)
		}
	}
	return filtered
}
