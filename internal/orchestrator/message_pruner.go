// message_pruner.go — conversation pruning for the ReAct loop.
// Delegates to agentloop.PruneMessages.
package orchestrator

import (
	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/models"
)

// pruneMessages removes middle messages to fit within token budget.
func (o *Orchestrator) pruneMessages(messages []models.Message, maxTokens int) []models.Message {
	return agentloop.PruneMessages(messages, maxTokens)
}
