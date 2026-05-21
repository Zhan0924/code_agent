package toollearn

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestAdaptivePolicy_RankTools_NoData(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	p := NewAdaptivePolicy(c)

	tools := []string{"read_file", "write_file", "run_workspace_cmd"}
	ranked := p.RankTools(tools, "")

	// With no data, order should be preserved (all get neutral 0.5 score)
	if len(ranked) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(ranked))
	}
}

func TestAdaptivePolicy_RankTools_BySuccessRate(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	p := NewAdaptivePolicy(c)

	// read_file: 100% success
	for range 5 {
		c.Record("read_file", []byte("{}"), true, 100*time.Millisecond, "", "s1")
	}
	// write_file: 60% success
	for range 3 {
		c.Record("write_file", []byte("{}"), true, 200*time.Millisecond, "", "s1")
	}
	for range 2 {
		c.Record("write_file", []byte("{}"), false, 200*time.Millisecond, "permission denied", "s1")
	}
	// run_workspace_cmd: 20% success
	for range 1 {
		c.Record("run_workspace_cmd", []byte("{}"), true, 500*time.Millisecond, "", "s1")
	}
	for range 4 {
		c.Record("run_workspace_cmd", []byte("{}"), false, 500*time.Millisecond, "exit code 1", "s1")
	}

	p.Update()

	tools := []string{"run_workspace_cmd", "write_file", "read_file"}
	ranked := p.RankTools(tools, "")

	if ranked[0] != "read_file" {
		t.Errorf("expected read_file first (highest success rate), got %s", ranked[0])
	}
	if ranked[len(ranked)-1] != "run_workspace_cmd" {
		t.Errorf("expected run_workspace_cmd last (lowest success rate), got %s", ranked[len(ranked)-1])
	}
}

func TestAdaptivePolicy_SequenceSuggestion(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	p := NewAdaptivePolicy(c)

	// Create successful sequence: read_file → edit_file (5 times)
	for i := 0; i < 5; i++ {
		sessionID := "seq1"
		c.Record("read_file", []byte("{}"), true, 100*time.Millisecond, "", sessionID)
		time.Sleep(1 * time.Millisecond)
		c.Record("edit_file", []byte("{}"), true, 150*time.Millisecond, "", sessionID)
	}

	p.Update()

	next := p.SuggestNext("read_file")
	if next != "edit_file" {
		t.Errorf("expected edit_file after read_file, got %s", next)
	}
}

func TestAdaptivePolicy_FormatContextHint(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	p := NewAdaptivePolicy(c)

	// Create failing tool
	for range 5 {
		c.Record("run_workspace_cmd", []byte("{}"), false, 500*time.Millisecond, "exit code 1", "s1")
	}

	p.Update()

	hint := p.FormatContextHint("")
	if hint == "" {
		t.Fatal("expected non-empty hint for failing tool")
	}
	if hint == "[Tool Learning Insights]\n" {
		t.Fatal("expected actual insights, got empty shell")
	}
}

func TestAdaptivePolicy_GetToolScore(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	p := NewAdaptivePolicy(c)

	// Too few samples
	for range 2 {
		c.Record("read_file", []byte("{}"), true, 100*time.Millisecond, "", "s1")
	}
	p.Update()
	if score := p.GetToolScore("read_file"); score != nil {
		t.Error("expected nil score with < 3 samples")
	}

	// Enough samples
	c.Record("read_file", []byte("{}"), true, 100*time.Millisecond, "", "s1")
	p.Update()
	score := p.GetToolScore("read_file")
	if score == nil {
		t.Fatal("expected non-nil score with 3 samples")
	}
	if score.SuccessRate != 1.0 {
		t.Errorf("expected success rate 1.0, got %f", score.SuccessRate)
	}
}
