package llm

import (
	"context"

	"github.com/agent/code_agent/internal/models"
)

// Message is a convenience alias for constructing chat messages
// without importing the models package directly.
type Message struct {
	Role    string
	Content string
}

// CompleteResponse is a simplified response from the Complete helper.
type CompleteResponse struct {
	Content string
}

// Complete is a convenience wrapper around ChatCompletion for simple
// single-turn requests (no tool calls, no streaming).
func (c *Client) Complete(ctx context.Context, msgs []Message, tools []models.ToolDefinition) (*CompleteResponse, error) {
	modelMsgs := make([]models.Message, len(msgs))
	for i, m := range msgs {
		modelMsgs[i] = models.Message{
			Role:    models.Role(m.Role),
			Content: m.Content,
		}
	}

	req := &ChatRequest{
		Messages: modelMsgs,
		Tools:    tools,
	}

	resp, err := c.ChatCompletion(ctx, req)
	if err != nil {
		return nil, err
	}

	return &CompleteResponse{Content: resp.Content}, nil
}
