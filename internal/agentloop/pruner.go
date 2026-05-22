package agentloop

import (
	"fmt"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
)

// PruneMessages removes middle messages to fit within token budget, keeping
// system prompt (first) and the most-recent exchanges that fit in 60% of the
// budget. Always keeps at least the last 4 messages.
func PruneMessages(messages []models.Message, maxTokens int) []models.Message {
	if len(messages) <= 5 {
		return messages
	}

	keepBudget := maxTokens * 60 / 100
	keep := 0
	accumulated := 0
	for i := len(messages) - 1; i >= 1; i-- {
		msgTokens := llm.EstimateTokens(messages[i].Content)
		if accumulated+msgTokens > keepBudget {
			break
		}
		accumulated += msgTokens
		keep++
	}
	if keep < 4 {
		keep = 4
	}
	if keep >= len(messages)-1 {
		keep = len(messages) - 1
	}

	prunedCount := len(messages) - 1 - keep
	prunedTools := 0
	for i := 1; i < len(messages)-keep; i++ {
		if messages[i].Role == models.RoleTool {
			prunedTools++
		}
	}

	result := make([]models.Message, 0, keep+2)
	result = append(result, messages[0])
	result = append(result, models.Message{
		Role: models.RoleSystem,
		Content: fmt.Sprintf(
			"[Context pruned: %d earlier messages removed (%d tool results). "+
				"Your .plan.md file and workspace files are still intact — use read_file to re-read them if needed. "+
				"Continue from where the remaining messages left off.]",
			prunedCount, prunedTools),
	})
	result = append(result, messages[len(messages)-keep:]...)
	return result
}
