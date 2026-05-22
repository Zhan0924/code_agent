package agentloop

import (
	"strings"
	"testing"
)

func TestTrajectoryMemory_RecordAndRetrieve(t *testing.T) {
	tm := NewTrajectoryMemory()
	tm.Record("code_fix", []string{"read_file", "grep", "edit_file"}, true)
	tm.Record("code_fix", []string{"list_dir", "read_file", "patch_file"}, true)
	tm.Record("code_fix", []string{"grep", "read_file"}, false) // failure, should not be retrieved

	matches := tm.Retrieve("code_fix")
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	// Most recent first
	if matches[0].Tools[0] != "list_dir" {
		t.Errorf("expected most recent match first, got %v", matches[0].Tools)
	}
}

func TestTrajectoryMemory_FormatHint(t *testing.T) {
	tm := NewTrajectoryMemory()
	tm.Record("code_query", []string{"rag_search", "read_file"}, true)

	hint := tm.FormatHint("code_query")
	if !strings.Contains(hint, "TRAJECTORY HINT") {
		t.Error("missing TRAJECTORY HINT marker")
	}
	if !strings.Contains(hint, "rag_search → read_file") {
		t.Error("missing tool sequence in hint")
	}

	// No match
	if hint := tm.FormatHint("unknown"); hint != "" {
		t.Error("expected empty hint for unknown intent")
	}
}

func TestTrajectoryMemory_SlidingWindow(t *testing.T) {
	tm := NewTrajectoryMemory()
	for i := range 60 {
		tm.Record("test", []string{"tool"}, i%2 == 0)
	}
	if len(tm.entries) != maxTrajectories {
		t.Errorf("expected cap at %d, got %d", maxTrajectories, len(tm.entries))
	}
}
