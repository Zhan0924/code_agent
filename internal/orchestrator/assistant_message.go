package orchestrator

import (
	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/models"
)

// assistantMessage builds a persisted assistant row with structured citation
// IDs captured at write time (REAUDIT-P1-4).
func assistantMessage(content string, toolCalls []models.ToolCall) models.Message {
	return models.Message{
		Role:           models.RoleAssistant,
		Content:        content,
		ToolCalls:      toolCalls,
		CitedMemoryIDs: memory.ParseCitationIDs(content),
	}
}
