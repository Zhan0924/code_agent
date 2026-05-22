package orchestrator

import (
	"strings"
	"testing"

	"github.com/agent/code_agent/internal/models"
)

func TestCompactEarlyMessages(t *testing.T) {
	longContent := strings.Repeat("x", 3000)

	messages := []models.Message{
		{Role: models.RoleUser, Content: "hello"},
		{Role: models.RoleTool, Content: longContent, ToolCallID: "1"},
		{Role: models.RoleTool, Content: longContent, ToolCallID: "2"},
		{Role: models.RoleTool, Content: longContent, ToolCallID: "3"},
		{Role: models.RoleTool, Content: longContent, ToolCallID: "4"},
		{Role: models.RoleTool, Content: longContent, ToolCallID: "5"},
		{Role: models.RoleTool, Content: longContent, ToolCallID: "6"},
	}

	compactEarlyMessages(messages, 5)

	// First 3 tool results should be compacted (6 total - 3 recent = 3 compacted)
	compactedCount := 0
	for _, m := range messages {
		if m.Role == models.RoleTool && strings.Contains(m.Content, "[compacted") {
			compactedCount++
		}
	}
	if compactedCount != 3 {
		t.Errorf("expected 3 compacted messages, got %d", compactedCount)
	}

	// Last 3 tool results should be intact
	for _, m := range messages[4:] {
		if m.Role == models.RoleTool && strings.Contains(m.Content, "[compacted") {
			t.Error("recent tool result should not be compacted")
		}
	}
}

func TestCompactEarlyMessages_NoOp(t *testing.T) {
	short := "short content"
	messages := []models.Message{
		{Role: models.RoleTool, Content: short, ToolCallID: "1"},
		{Role: models.RoleTool, Content: short, ToolCallID: "2"},
	}

	compactEarlyMessages(messages, 5)

	for _, m := range messages {
		if strings.Contains(m.Content, "[compacted") {
			t.Error("short messages should not be compacted")
		}
	}
}
