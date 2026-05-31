package orchestrator

import (
	"github.com/agent/code_agent/internal/models"
)

// toolMetadata returns the (cached) ToolDefinition for a tool name, with the
// behavior bits (IsFileWrite / IsIdempotentRead / TriggersAutoTest /
// InvalidatesCache) populated. Returns zero value + false when the tool is
// not in the registry.
//
// This is the single source of truth that replaces what used to be 9 separate
// hardcoded tool-name lists scattered across:
//   - react_core.go (auto-test trigger)
//   - orchestrator.go (captureForTransaction)
//   - speculative_cache.go (idempotent / invalidates)
//   - multiagent/supervisor.go (file-write conflict detection)
//   - api/dynamic_tool_handlers.go (builtin name collision)
//   - api/mcp_skill_handlers.go (builtin list enumeration)
//
// Each consumer queries metadata here instead of pattern-matching on names.
func (o *Orchestrator) toolMetadata(name string) (models.ToolDefinition, bool) {
	if o == nil || o.toolRegistry == nil {
		return models.ToolDefinition{}, false
	}
	t, ok := o.toolRegistry.Get(name)
	if !ok {
		return models.ToolDefinition{}, false
	}
	return t.Definition(), true
}

// IsFileWriteTool reports whether the named tool mutates files. Driven by the
// IsFileWrite metadata bit set in fileToolDefinitions / gitToolDefinitions /
// lsp_tools. Exported so external packages (e.g. main wiring, multiagent
// supervisor classifier) can share the same classification.
func (o *Orchestrator) IsFileWriteTool(name string) bool {
	def, ok := o.toolMetadata(name)
	return ok && def.IsFileWrite
}

// isFileWriteTool is the unexported alias kept for internal call sites.
func (o *Orchestrator) isFileWriteTool(name string) bool {
	return o.IsFileWriteTool(name)
}

// triggersAutoTest reports whether successful execution of the named tool
// should trigger the auto-test runner.
func (o *Orchestrator) triggersAutoTest(name string) bool {
	def, ok := o.toolMetadata(name)
	return ok && def.TriggersAutoTest
}

// BuiltinToolNames returns the set of builtin tool names currently registered.
// Replaces the hardcoded `builtins` slices in dynamic_tool_handlers and
// mcp_skill_handlers — those used to drift from reality whenever a new
// builtin was added. The result is suitable for membership tests; the
// returned slice is freshly allocated and safe to mutate.
func (o *Orchestrator) BuiltinToolNames() []string {
	if o == nil || o.toolRegistry == nil {
		return nil
	}
	defs := o.toolRegistry.Definitions()
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		if d.Source == "builtin" {
			out = append(out, d.Name)
		}
	}
	return out
}

// IsBuiltinTool returns true when the named tool is a registered builtin.
func (o *Orchestrator) IsBuiltinTool(name string) bool {
	def, ok := o.toolMetadata(name)
	return ok && def.Source == "builtin"
}
