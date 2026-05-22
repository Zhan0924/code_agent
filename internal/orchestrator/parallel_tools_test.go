package orchestrator

import (
	"testing"

	"github.com/agent/code_agent/internal/models"
)

func TestCanParallelExecute(t *testing.T) {
	tests := []struct {
		name     string
		calls    []models.ToolCall
		expected bool
	}{
		{"single call", []models.ToolCall{{Name: "read_file"}}, false},
		{"empty", nil, false},
		{"all idempotent", []models.ToolCall{{Name: "read_file"}, {Name: "grep"}, {Name: "list_dir"}}, true},
		{"mixed", []models.ToolCall{{Name: "read_file"}, {Name: "write_file"}}, false},
		{"all write", []models.ToolCall{{Name: "edit_file"}, {Name: "write_file"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canParallelExecute(tt.calls); got != tt.expected {
				t.Errorf("canParallelExecute() = %v, want %v", got, tt.expected)
			}
		})
	}
}
