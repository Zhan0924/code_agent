package agentloop

import (
	"context"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
)

// LLMCaller abstracts an LLM provider for the ReAct loop.
// llm.Client satisfies this interface directly.
type LLMCaller interface {
	ChatCompletion(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
}

// ToolExecutor executes a single tool call and returns the result.
type ToolExecutor interface {
	Execute(ctx context.Context, tc models.ToolCall) (*models.ToolResult, error)
}

// ToolProvider supplies the list of tool definitions available for the loop.
type ToolProvider interface {
	Definitions() []models.ToolDefinition
}

// EventSink receives structured events during loop execution.
type EventSink interface {
	Emit(event models.ReactStreamEvent)
}

// NoopSink discards all events.
type NoopSink struct{}

func (NoopSink) Emit(models.ReactStreamEvent) {}

// ChannelSink sends events into a buffered channel.
type ChannelSink struct {
	Ch chan<- models.ReactStreamEvent
}

func (s *ChannelSink) Emit(e models.ReactStreamEvent) { s.Ch <- e }
