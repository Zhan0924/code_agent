// message_pruner.go — conversation pruning for the ReAct loop.
//
// This is the "hot path" pruner: it runs once per ReAct iteration on messages
// that are about to be sent to the LLM. It's intentionally more aggressive
// about *keeping recent tool exchanges* than the general-purpose
// internal/context.TokenPruner because the ReAct loop's correctness depends
// on the LLM still being able to see the last few tool-call / tool-result
// pairs verbatim. If those are evicted, the model typically loops or
// confabulates a non-existent tool.
//
// Strategy:
//  1. System prompt (messages[0]) is always kept.
//  2. Starting from the tail, keep as many recent messages as fit within
//     60% of the total budget (the rest is for system + padding).
//  3. A synthetic "[Context pruned: N messages dropped]" system message is
//     inserted between the real system prompt and the retained tail so the
//     LLM knows context was trimmed and can request files via read_file.
//
// Extracted from orchestrator.go as part of the file-split refactor.
package orchestrator

import (
	"fmt"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
)

// pruneMessages removes middle messages to fit within token budget, keeping
// system prompt (first) and the most-recent exchanges that fit in 60% of the
// budget. Always keeps at least the last 4 messages to preserve at least two
// tool-call / tool-result pairs.
func (o *Orchestrator) pruneMessages(messages []models.Message, maxTokens int) []models.Message {
	if len(messages) <= 5 {
		return messages
	}

	// Dynamic keep count: scan from the end, accumulating tokens until we
	// hit 60% of maxTokens. This keeps as many recent messages as possible.
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
		keep = 4 // minimum: keep at least 2 tool call/result pairs
	}
	if keep >= len(messages)-1 {
		keep = len(messages) - 1
	}

	// Count pruned messages (for the summary notice) and specifically the
	// number of tool results dropped (the LLM especially needs to know).
	prunedCount := len(messages) - 1 - keep
	prunedTools := 0
	for i := 1; i < len(messages)-keep; i++ {
		if messages[i].Role == models.RoleTool {
			prunedTools++
		}
	}

	result := make([]models.Message, 0, keep+2)
	result = append(result, messages[0]) // original system prompt
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
