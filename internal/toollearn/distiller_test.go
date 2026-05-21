package toollearn

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestDistiller_Distill_NoData(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	d := NewDistiller(c, zap.NewNop())

	n := d.Distill()
	if n != 0 {
		t.Errorf("expected 0 strategies from empty data, got %d", n)
	}
}

func TestDistiller_Distill_SuccessfulSession(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	d := NewDistiller(c, zap.NewNop())
	d.minSamples = 1

	// Simulate a successful session
	session := "sess-1"
	tools := []string{"read_file", "edit_file", "run_tests"}
	base := time.Now()
	for i, tool := range tools {
		c.Record(tool, []byte("{}"), true, 100*time.Millisecond, "", session)
		// Ensure ordering
		c.mu.Lock()
		c.buffer[len(c.buffer)-1].CreatedAt = base.Add(time.Duration(i) * time.Second)
		c.mu.Unlock()
	}

	n := d.Distill()
	if n != 1 {
		t.Errorf("expected 1 new strategy, got %d", n)
	}

	strategies := d.Strategies()
	if len(strategies) == 0 {
		t.Fatal("expected at least one strategy")
	}
	if strategies[0].TaskPattern != "implement_and_verify" {
		t.Errorf("expected implement_and_verify pattern, got %s", strategies[0].TaskPattern)
	}
}

func TestDistiller_Distill_FailedSessionIgnored(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	d := NewDistiller(c, zap.NewNop())
	d.minSamples = 1

	session := "sess-fail"
	base := time.Now()
	for i := range 5 {
		c.Record("read_file", []byte("{}"), false, 100*time.Millisecond, "error", session)
		c.mu.Lock()
		c.buffer[len(c.buffer)-1].CreatedAt = base.Add(time.Duration(i) * time.Second)
		c.mu.Unlock()
	}

	n := d.Distill()
	if n != 0 {
		t.Errorf("expected 0 strategies from failed session, got %d", n)
	}
}

func TestDistiller_Recommend(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	d := NewDistiller(c, zap.NewNop())
	d.minSamples = 1

	// Manually insert a strategy
	d.mu.Lock()
	d.strategies["code_modification"] = &StrategyEntry{
		TaskPattern: "code_modification",
		ToolChain:   []string{"read_file", "edit_file"},
		SuccessRate: 0.9,
		SampleCount: 10,
	}
	d.mu.Unlock()

	strat := d.Recommend("fix the bug in handler")
	if strat == nil {
		t.Fatal("expected a recommendation")
	}
	if strat.TaskPattern != "code_modification" {
		t.Errorf("expected code_modification, got %s", strat.TaskPattern)
	}
}

func TestDistiller_FormatRecommendation(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	d := NewDistiller(c, zap.NewNop())
	d.minSamples = 1

	d.mu.Lock()
	d.strategies["implement_and_verify"] = &StrategyEntry{
		TaskPattern: "implement_and_verify",
		ToolChain:   []string{"read_file", "write_file", "run_tests"},
		SuccessRate: 0.85,
		SampleCount: 8,
	}
	d.mu.Unlock()

	hint := d.FormatRecommendation("implement a new feature")
	if hint == "" {
		t.Fatal("expected non-empty recommendation")
	}
	if !contains(hint, "implement_and_verify") {
		t.Errorf("expected pattern name in hint, got: %s", hint)
	}
}

func TestDistiller_NoRecommendBelowMinSamples(t *testing.T) {
	c := NewCollector(nil, zap.NewNop())
	d := NewDistiller(c, zap.NewNop())

	d.mu.Lock()
	d.strategies["testing"] = &StrategyEntry{
		TaskPattern: "testing",
		ToolChain:   []string{"run_tests"},
		SuccessRate: 1.0,
		SampleCount: 2, // below default minSamples=5
	}
	d.mu.Unlock()

	strat := d.Recommend("run tests")
	if strat != nil {
		t.Error("expected nil recommendation below minSamples")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
