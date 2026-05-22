package orchestrator

import (
	"strings"
	"testing"
)

func TestMicroPlanPrompt(t *testing.T) {
	// Too early — no plan
	if p := microPlanPrompt(1, 20); p != nil {
		t.Error("should not trigger at step 1")
	}

	// Trigger step
	p := microPlanPrompt(3, 20)
	if p == nil {
		t.Fatal("should trigger at step 3")
	}
	if !strings.Contains(p.Content, "MICRO-PLAN") {
		t.Error("missing MICRO-PLAN marker")
	}

	// Next interval
	if p := microPlanPrompt(4, 20); p != nil {
		t.Error("should not trigger at step 4")
	}
	if p := microPlanPrompt(9, 20); p == nil {
		t.Error("should trigger at step 9 (3+6)")
	}
}
