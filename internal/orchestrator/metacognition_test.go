package orchestrator

import (
	"strings"
	"testing"
)

func TestMetacognitiveState_InitialState(t *testing.T) {
	m := NewMetacognitiveState()
	if m.Confidence != 0.7 {
		t.Errorf("expected initial confidence 0.7, got %f", m.Confidence)
	}
	if m.StuckScore != 0.0 {
		t.Errorf("expected initial stuck score 0.0, got %f", m.StuckScore)
	}
}

func TestMetacognitiveState_ConfidenceAfterSuccesses(t *testing.T) {
	m := NewMetacognitiveState()
	for i := range 5 {
		m.RecordOutcome("read_file", true, false)
		_ = i
	}
	if m.Confidence != 1.0 {
		t.Errorf("expected confidence 1.0 after all successes, got %f", m.Confidence)
	}
	if m.StuckScore != 0.0 {
		t.Errorf("expected stuck score 0.0 after all successes, got %f", m.StuckScore)
	}
}

func TestMetacognitiveState_ConfidenceDropsOnFailures(t *testing.T) {
	m := NewMetacognitiveState()
	m.RecordOutcome("read_file", true, false)
	m.RecordOutcome("read_file", true, false)
	m.RecordOutcome("run_workspace_cmd", false, false)
	m.RecordOutcome("run_workspace_cmd", false, true)
	m.RecordOutcome("run_workspace_cmd", false, true)

	if m.Confidence >= 0.5 {
		t.Errorf("expected confidence < 0.5 after multiple failures, got %f", m.Confidence)
	}
}

func TestMetacognitiveState_StuckScoreRises(t *testing.T) {
	m := NewMetacognitiveState()
	m.RecordOutcome("read_file", true, false)
	m.RecordOutcome("run_workspace_cmd", false, false)
	m.RecordOutcome("run_workspace_cmd", false, true)
	m.RecordOutcome("run_workspace_cmd", false, true)
	m.RecordOutcome("run_workspace_cmd", false, true)

	if m.StuckScore < 0.5 {
		t.Errorf("expected stuck score >= 0.5 after consecutive failures, got %f", m.StuckScore)
	}
}

func TestMetacognitiveState_NeedsReflection(t *testing.T) {
	m := NewMetacognitiveState()
	// All successes — no reflection needed
	for i := range 5 {
		m.RecordOutcome("read_file", true, false)
		_ = i
	}
	if m.NeedsReflection() {
		t.Error("should not need reflection after all successes")
	}

	// Multiple failures — should trigger reflection
	for i := range 5 {
		m.RecordOutcome("run_workspace_cmd", false, true)
		_ = i
	}
	if !m.NeedsReflection() {
		t.Error("should need reflection after multiple failures")
	}
}

func TestMetacognitiveState_ShouldRequestClarification(t *testing.T) {
	m := NewMetacognitiveState()
	// Not enough steps yet
	m.RecordOutcome("read_file", false, false)
	if m.ShouldRequestClarification() {
		t.Error("should not request clarification with < 5 steps")
	}

	// After 5+ steps with low confidence
	for i := range 5 {
		m.RecordOutcome("run_workspace_cmd", false, true)
		_ = i
	}
	if !m.ShouldRequestClarification() {
		t.Error("should request clarification after 5+ steps with low confidence")
	}
}

func TestMetacognitiveState_AddUncertainty(t *testing.T) {
	m := NewMetacognitiveState()
	m.AddUncertainty("file path unknown")
	m.AddUncertainty("API schema unclear")
	m.AddUncertainty("file path unknown") // duplicate

	if len(m.UncertainAreas) != 2 {
		t.Errorf("expected 2 unique uncertainties, got %d", len(m.UncertainAreas))
	}
}

func TestMetacognitiveState_AddAssumption(t *testing.T) {
	m := NewMetacognitiveState()
	m.AddAssumption("assuming Go 1.22+")
	m.AddAssumption("assuming Redis is running")

	if len(m.AssumptionsMade) != 2 {
		t.Errorf("expected 2 assumptions, got %d", len(m.AssumptionsMade))
	}
}

func TestMetacognitiveState_AdaptiveReflectionMessage(t *testing.T) {
	m := NewMetacognitiveState()
	for i := range 5 {
		m.RecordOutcome("run_workspace_cmd", false, true)
		_ = i
	}
	m.AddUncertainty("file path unknown")
	m.AddAssumption("assuming Go 1.22+")

	msg := m.AdaptiveReflectionMessage(10, 50)
	if msg == nil {
		t.Fatal("expected non-nil message")
	}
	if msg.Role != "system" {
		t.Errorf("expected system role, got %s", msg.Role)
	}
	content := msg.Content
	if content == "" {
		t.Error("expected non-empty content")
	}
	// Should mention stuck state
	if m.StuckScore > 0.5 && !strings.Contains(content, "stuck") && !strings.Contains(content, "STOP") {
		t.Error("expected stuck warning in message")
	}
	// Should mention uncertainties
	if !strings.Contains(content, "uncertainties") && !strings.Contains(content, "file path unknown") {
		t.Error("expected uncertainties in message")
	}
}
