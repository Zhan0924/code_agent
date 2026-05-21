// Package tools provides a uniform abstraction for every capability the Agent
// can invoke during a ReAct loop — built-in (execute_code, search_code, file
// tools), remote MCP tools, runtime-registered skills, and future LSP tools.
//
// Before this package existed, `orchestrator.getAvailableTools()` and
// `orchestrator.executeTool()` hard-coded the dispatch through a chain of
// if/else branches. Adding a new provider required editing orchestrator.go
// and mechanically threading through the dependency. This registry replaces
// that with a clean plug-in point:
//
//	reg := tools.NewRegistry()
//	reg.Register(myTool)          // one or more tools
//	reg.RegisterProvider(mcpGw)   // batch-register a whole provider
//	defs  := reg.Definitions()    // → LLM function-calling schema
//	result, err := reg.Execute(ctx, "edit_file", rawArgs)
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/metrics"
	"github.com/agent/code_agent/internal/models"
)

// ErrToolNotFound is returned by Execute when a tool name is unknown.
var ErrToolNotFound = errors.New("tool not found")

// Tool is the contract every callable capability must satisfy. Implementations
// are expected to be safe for concurrent invocation.
type Tool interface {
	// Definition returns the LLM-facing function-calling schema for this tool.
	Definition() models.ToolDefinition
	// Execute runs the tool with the given JSON-encoded arguments.
	Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error)
}

// Provider yields a batch of Tools. The registry uses this to fold an entire
// sub-system (e.g. MCP gateway, skill catalog) into its index in one call.
type Provider interface {
	Name() string
	Tools() []Tool
}

// Registry is a concurrent-safe index of named tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds one tool. It is an error to register two tools under the same
// name — callers should use distinct names (often prefixed by their source,
// e.g. "mcp_github_create_issue").
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return errors.New("nil tool")
	}
	def := t.Definition()
	if def.Name == "" {
		return errors.New("tool definition missing name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[def.Name]; exists {
		return fmt.Errorf("tool %q already registered", def.Name)
	}
	r.tools[def.Name] = t
	return nil
}

// RegisterProvider imports all tools from a Provider, skipping duplicates and
// returning the number of tools successfully added.
func (r *Registry) RegisterProvider(p Provider) int {
	if p == nil {
		return 0
	}
	n := 0
	for _, t := range p.Tools() {
		if err := r.Register(t); err == nil {
			n++
		}
	}
	return n
}

// Unregister removes a tool by name; returns true if it existed.
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; ok {
		delete(r.tools, name)
		return true
	}
	return false
}

// Get retrieves a registered tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Definitions returns the schema of every registered tool, sorted by name for
// deterministic LLM prompts (stable cache keys).
func (r *Registry) Definitions() []models.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]models.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.Definition())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Execute looks up a tool by name, invokes it, and records metrics (latency +
// outcome counters partitioned by source tag).
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (*models.ToolResult, error) {
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrToolNotFound, name)
	}
	start := time.Now()
	source := t.Definition().Source
	if source == "" {
		source = "unknown"
	}
	result, err := t.Execute(ctx, args)
	elapsed := time.Since(start).Seconds()
	status := "success"
	if err != nil {
		status = "error"
	} else if result != nil && result.IsError {
		status = "tool_error"
	}
	metrics.ToolExecutionDuration.WithLabelValues(name, source, status).Observe(elapsed)
	metrics.ToolExecutionTotal.WithLabelValues(name, source, status).Inc()
	return result, err
}
