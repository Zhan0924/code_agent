package orchestrator

import (
	"testing"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// TestDedupeToolCalls covers the cases that motivated the helper:
// exact duplicate (same JSON args, same name), key-order-different JSON
// (still a duplicate), distinct args (not a duplicate), distinct names with
// identical args (not a duplicate), and the malformed-args fallback path.
func TestDedupeToolCalls(t *testing.T) {
	tests := []struct {
		name string
		in   []models.ToolCall
		want []string // expected IDs in order
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			name: "single",
			in:   []models.ToolCall{{ID: "1", Name: "read_file", Args: []byte(`{"path":"a"}`)}},
			want: []string{"1"},
		},
		{
			name: "exact_duplicate",
			in: []models.ToolCall{
				{ID: "1", Name: "run_workspace_cmd", Args: []byte(`{"command":"go version"}`)},
				{ID: "2", Name: "run_workspace_cmd", Args: []byte(`{"command":"go version"}`)},
			},
			want: []string{"1"},
		},
		{
			name: "key_order_different",
			in: []models.ToolCall{
				{ID: "1", Name: "write_file", Args: []byte(`{"path":"a","content":"x"}`)},
				{ID: "2", Name: "write_file", Args: []byte(`{"content":"x","path":"a"}`)},
			},
			want: []string{"1"},
		},
		{
			name: "distinct_args",
			in: []models.ToolCall{
				{ID: "1", Name: "read_file", Args: []byte(`{"path":"a"}`)},
				{ID: "2", Name: "read_file", Args: []byte(`{"path":"b"}`)},
			},
			want: []string{"1", "2"},
		},
		{
			name: "distinct_names_same_args",
			in: []models.ToolCall{
				{ID: "1", Name: "read_file", Args: []byte(`{"path":"a"}`)},
				{ID: "2", Name: "list_files", Args: []byte(`{"path":"a"}`)},
			},
			want: []string{"1", "2"},
		},
		{
			name: "mixed_keep_first_each_group",
			in: []models.ToolCall{
				{ID: "1", Name: "read_file", Args: []byte(`{"path":"a"}`)},
				{ID: "2", Name: "read_file", Args: []byte(`{"path":"a"}`)},
				{ID: "3", Name: "read_file", Args: []byte(`{"path":"b"}`)},
				{ID: "4", Name: "read_file", Args: []byte(`{"path":"a"}`)},
			},
			want: []string{"1", "3"},
		},
		{
			name: "malformed_args_fallback",
			in: []models.ToolCall{
				{ID: "1", Name: "x", Args: []byte(`not-json`)},
				{ID: "2", Name: "x", Args: []byte(`not-json`)},
				{ID: "3", Name: "x", Args: []byte(`also-not-json`)},
			},
			want: []string{"1", "3"},
		},
		{
			name: "nested_object_key_order",
			in: []models.ToolCall{
				{ID: "1", Name: "edit_file", Args: []byte(`{"path":"a","meta":{"foo":1,"bar":2}}`)},
				{ID: "2", Name: "edit_file", Args: []byte(`{"meta":{"bar":2,"foo":1},"path":"a"}`)},
			},
			want: []string{"1"},
		},
		{
			name: "empty_args_both_collapse",
			in: []models.ToolCall{
				{ID: "1", Name: "x", Args: nil},
				{ID: "2", Name: "x", Args: nil},
			},
			want: []string{"1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupeToolCalls(tt.in, zap.NewNop())
			if len(got) != len(tt.want) {
				t.Fatalf("len mismatch: got %d (%v) want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i].ID != tt.want[i] {
					t.Errorf("at %d: got ID %q, want %q", i, got[i].ID, tt.want[i])
				}
			}
		})
	}
}

// TestDedupeToolCalls_NilLoggerOK guards the nil-logger path so a future
// caller wiring the helper into a test scaffold without a zap instance
// doesn't trip a nil deref.
func TestDedupeToolCalls_NilLoggerOK(t *testing.T) {
	in := []models.ToolCall{
		{ID: "1", Name: "x", Args: []byte(`{"a":1}`)},
		{ID: "2", Name: "x", Args: []byte(`{"a":1}`)},
	}
	out := dedupeToolCalls(in, nil)
	if len(out) != 1 {
		t.Fatalf("expected 1, got %d", len(out))
	}
}
