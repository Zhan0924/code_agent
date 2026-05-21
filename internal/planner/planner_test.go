package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.uber.org/zap"
)

// ─── Mock LLM ───────────────────────────────────────────────────────────────

type mockLLM struct {
	response string
	err      error
}

func (m *mockLLM) Call(_ context.Context, _, _ string) (string, error) {
	return m.response, m.err
}

// ─── DAG Validation Tests ───────────────────────────────────────────────────

func TestValidateDAG_Valid(t *testing.T) {
	steps := []Step{
		{ID: "step_1", Action: "read_file"},
		{ID: "step_2", Action: "edit_file", DependsOn: []string{"step_1"}},
		{ID: "step_3", Action: "run_tests", DependsOn: []string{"step_2"}},
	}
	if err := ValidateDAG(steps); err != nil {
		t.Errorf("expected valid DAG, got error: %v", err)
	}
}

func TestValidateDAG_DuplicateIDs(t *testing.T) {
	steps := []Step{
		{ID: "step_1", Action: "read_file"},
		{ID: "step_1", Action: "write_file"},
	}
	err := ValidateDAG(steps)
	if err == nil {
		t.Fatal("expected error for duplicate IDs")
	}
	if !contains(err.Error(), "duplicate") {
		t.Errorf("expected 'duplicate' in error, got: %s", err)
	}
}

func TestValidateDAG_UnknownDependency(t *testing.T) {
	steps := []Step{
		{ID: "step_1", Action: "read_file", DependsOn: []string{"nonexistent"}},
	}
	err := ValidateDAG(steps)
	if err == nil {
		t.Fatal("expected error for unknown dependency")
	}
	if !contains(err.Error(), "unknown") {
		t.Errorf("expected 'unknown' in error, got: %s", err)
	}
}

func TestValidateDAG_SelfDependency(t *testing.T) {
	steps := []Step{
		{ID: "step_1", Action: "read_file", DependsOn: []string{"step_1"}},
	}
	err := ValidateDAG(steps)
	if err == nil {
		t.Fatal("expected error for self-dependency")
	}
}

func TestValidateDAG_Cycle(t *testing.T) {
	steps := []Step{
		{ID: "a", Action: "x", DependsOn: []string{"b"}},
		{ID: "b", Action: "x", DependsOn: []string{"c"}},
		{ID: "c", Action: "x", DependsOn: []string{"a"}},
	}
	err := ValidateDAG(steps)
	if err == nil {
		t.Fatal("expected error for cycle")
	}
	if !contains(err.Error(), "cycle") {
		t.Errorf("expected 'cycle' in error, got: %s", err)
	}
}

func TestValidateDAG_Empty(t *testing.T) {
	if err := ValidateDAG(nil); err != nil {
		t.Errorf("expected nil error for empty steps, got: %v", err)
	}
}

// ─── Topological Sort Tests ─────────────────────────────────────────────────

func TestTopologicalSort_Linear(t *testing.T) {
	steps := []Step{
		{ID: "a", DependsOn: nil},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "c", DependsOn: []string{"b"}},
	}
	levels, err := TopologicalSort(steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 3 {
		t.Errorf("expected 3 levels, got %d", len(levels))
	}
	if levels[0][0].ID != "a" {
		t.Errorf("expected first level to be 'a', got %s", levels[0][0].ID)
	}
}

func TestTopologicalSort_Parallel(t *testing.T) {
	steps := []Step{
		{ID: "a"},
		{ID: "b"},
		{ID: "c", DependsOn: []string{"a", "b"}},
	}
	levels, err := TopologicalSort(steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 2 {
		t.Errorf("expected 2 levels (a,b parallel then c), got %d", len(levels))
	}
	// First level should have 2 steps
	if len(levels[0]) != 2 {
		t.Errorf("expected 2 parallel steps in level 0, got %d", len(levels[0]))
	}
}

func TestTopologicalSort_Diamond(t *testing.T) {
	// A → B, A → C, B → D, C → D
	steps := []Step{
		{ID: "A"},
		{ID: "B", DependsOn: []string{"A"}},
		{ID: "C", DependsOn: []string{"A"}},
		{ID: "D", DependsOn: []string{"B", "C"}},
	}
	levels, err := TopologicalSort(steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 3 {
		t.Errorf("expected 3 levels, got %d", len(levels))
	}
}

// ─── Plan Parsing Tests ─────────────────────────────────────────────────────

func TestParsePlanJSON_Valid(t *testing.T) {
	raw := `{
		"goal": "Fix the bug",
		"reasoning": "Need to read, edit, test",
		"steps": [
			{"id": "step_1", "action": "read_file", "description": "Read main.go"},
			{"id": "step_2", "action": "edit_file", "description": "Fix bug", "depends_on": ["step_1"]}
		]
	}`
	plan, err := parsePlanJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Goal != "Fix the bug" {
		t.Errorf("expected goal 'Fix the bug', got %q", plan.Goal)
	}
	if len(plan.Steps) != 2 {
		t.Errorf("expected 2 steps, got %d", len(plan.Steps))
	}
	// All steps should default to pending
	for _, s := range plan.Steps {
		if s.Status != StepPending {
			t.Errorf("expected status pending, got %s", s.Status)
		}
	}
}

func TestParsePlanJSON_WithCodeFence(t *testing.T) {
	raw := "Here's the plan:\n```json\n{\"goal\":\"test\",\"steps\":[],\"reasoning\":\"\"}\n```\nDone."
	plan, err := parsePlanJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Goal != "test" {
		t.Errorf("expected goal 'test', got %q", plan.Goal)
	}
}

func TestParsePlanJSON_Invalid(t *testing.T) {
	_, err := parsePlanJSON("not json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ─── Plan Methods Tests ─────────────────────────────────────────────────────

func TestPlan_IsComplete(t *testing.T) {
	plan := &Plan{
		Steps: []Step{
			{ID: "a", Status: StepCompleted},
			{ID: "b", Status: StepSkipped},
		},
	}
	if !plan.IsComplete() {
		t.Error("expected plan to be complete")
	}

	plan.Steps[0].Status = StepPending
	if plan.IsComplete() {
		t.Error("expected plan to be incomplete")
	}
}

func TestPlan_FailedSteps(t *testing.T) {
	plan := &Plan{
		Steps: []Step{
			{ID: "a", Status: StepCompleted},
			{ID: "b", Status: StepFailed},
			{ID: "c", Status: StepFailed},
		},
	}
	failed := plan.FailedSteps()
	if len(failed) != 2 {
		t.Errorf("expected 2 failed steps, got %d", len(failed))
	}
}

func TestPlan_CompletedStepIDs(t *testing.T) {
	plan := &Plan{
		Steps: []Step{
			{ID: "a", Status: StepCompleted},
			{ID: "b", Status: StepFailed},
			{ID: "c", Status: StepCompleted},
		},
	}
	ids := plan.CompletedStepIDs()
	if len(ids) != 2 {
		t.Errorf("expected 2 completed IDs, got %d", len(ids))
	}
}

func TestPlan_Summary(t *testing.T) {
	plan := &Plan{
		Goal:      "Test plan",
		Version:   1,
		Reasoning: "For testing",
		Steps: []Step{
			{ID: "a", Action: "read_file", Description: "Read config", Status: StepCompleted},
			{ID: "b", Action: "edit_file", Description: "Fix bug", Status: StepFailed, DependsOn: []string{"a"}},
		},
	}
	summary := plan.Summary()
	if !contains(summary, "Test plan") {
		t.Error("summary should contain goal")
	}
	if !contains(summary, "✅") {
		t.Error("summary should contain completed emoji")
	}
	if !contains(summary, "❌") {
		t.Error("summary should contain failed emoji")
	}
}

func TestPlan_Serialization(t *testing.T) {
	plan := &Plan{
		ID:   "plan_123",
		Goal: "Test",
		Steps: []Step{
			{ID: "a", Action: "read_file", Status: StepPending},
		},
		Version: 1,
	}
	data, err := plan.ToJSON()
	if err != nil {
		t.Fatal(err)
	}

	restored, err := PlanFromJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != plan.ID {
		t.Errorf("expected ID %q, got %q", plan.ID, restored.ID)
	}
	if restored.Goal != plan.Goal {
		t.Errorf("expected goal %q, got %q", plan.Goal, restored.Goal)
	}
}

// ─── Complexity Estimation Tests ────────────────────────────────────────────

func TestEstimateComplexity_Simple(t *testing.T) {
	score := EstimateComplexity("What is this function?")
	if score >= 5 {
		t.Errorf("simple question should have low complexity, got %d", score)
	}
}

func TestEstimateComplexity_Complex(t *testing.T) {
	score := EstimateComplexity("Please refactor multiple files to implement a new feature, then run tests to verify everything works")
	if score < 5 {
		t.Errorf("complex task should have high complexity, got %d", score)
	}
}

func TestNeedsPlanning(t *testing.T) {
	tests := []struct {
		msg    string
		expect bool
	}{
		{"What does this do?", false},
		{"Refactor multiple files to implement authentication, then add tests and verify", true},
	}
	for _, tt := range tests {
		name := tt.msg
		if len(name) > 20 {
			name = name[:20]
		}
		t.Run(name, func(t *testing.T) {
			got := NeedsPlanning(tt.msg)
			if got != tt.expect {
				t.Errorf("NeedsPlanning(%q) = %v, want %v (score=%d)", tt.msg, got, tt.expect, EstimateComplexity(tt.msg))
			}
		})
	}
}

// ─── Planner Integration Test (with mock LLM) ──────────────────────────────

func TestPlanner_CreatePlan(t *testing.T) {
	mockResponse := `{
		"goal": "Add auth middleware",
		"reasoning": "Need to create middleware and test it",
		"steps": [
			{"id": "step_1", "action": "read_file", "description": "Read existing middleware"},
			{"id": "step_2", "action": "write_file", "description": "Create auth middleware", "depends_on": ["step_1"]},
			{"id": "step_3", "action": "run_tests", "description": "Run tests", "depends_on": ["step_2"]}
		]
	}`

	llm := &mockLLM{response: mockResponse}
	p := NewPlanner(llm, zapNop())

	plan, err := p.CreatePlan(context.Background(), "Add auth middleware", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Goal != "Add auth middleware" {
		t.Errorf("expected goal, got %q", plan.Goal)
	}
	if len(plan.Steps) != 3 {
		t.Errorf("expected 3 steps, got %d", len(plan.Steps))
	}
	if plan.Version != 1 {
		t.Errorf("expected version 1, got %d", plan.Version)
	}
}

func TestPlanner_CreatePlan_LLMError(t *testing.T) {
	llm := &mockLLM{err: fmt.Errorf("API timeout")}
	p := NewPlanner(llm, zapNop())

	_, err := p.CreatePlan(context.Background(), "task", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "LLM call failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPlanner_RevisePlan(t *testing.T) {
	mockResponse := `{
		"goal": "Revised plan",
		"reasoning": "Fixed the failing step",
		"steps": [
			{"id": "step_1", "action": "read_file", "description": "Read config", "status": "completed"},
			{"id": "step_2", "action": "edit_file", "description": "Different fix", "depends_on": ["step_1"]}
		]
	}`

	llm := &mockLLM{response: mockResponse}
	p := NewPlanner(llm, zapNop())

	original := &Plan{
		ID:      "plan_old",
		Goal:    "Original",
		Version: 1,
		Steps: []Step{
			{ID: "step_1", Status: StepCompleted},
			{ID: "step_2", Status: StepFailed},
		},
	}

	revised, err := p.RevisePlan(context.Background(), original, "step_2 failed: syntax error")
	if err != nil {
		t.Fatal(err)
	}
	if revised.Version != 2 {
		t.Errorf("expected version 2, got %d", revised.Version)
	}
	if revised.ID != original.ID {
		t.Errorf("expected same plan ID")
	}
	if revised.RevisedAt == nil {
		t.Error("expected RevisedAt to be set")
	}
}

// ─── Executor Tests ─────────────────────────────────────────────────────────

func TestExecutor_AllStepsSucceed(t *testing.T) {
	llm := &mockLLM{}
	p := NewPlanner(llm, zapNop())

	stepExec := func(_ context.Context, step Step) (string, error) {
		return "output of " + step.ID, nil
	}

	exec := NewExecutor(p, stepExec, zapNop())

	plan := &Plan{
		ID:   "test_plan",
		Goal: "Test",
		Steps: []Step{
			{ID: "a", Action: "read_file", Status: StepPending},
			{ID: "b", Action: "edit_file", Status: StepPending, DependsOn: []string{"a"}},
		},
		Version: 1,
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Errorf("expected success, got: %s", result.Summary)
	}
	if len(result.StepOutputs) != 2 {
		t.Errorf("expected 2 outputs, got %d", len(result.StepOutputs))
	}
}

func TestExecutor_StepFailsAndRevises(t *testing.T) {
	callCount := 0
	revisionResponse := `{
		"goal": "Revised",
		"reasoning": "Fixed",
		"steps": [
			{"id": "a", "action": "read_file", "status": "completed"},
			{"id": "b_fix", "action": "edit_file", "depends_on": ["a"]}
		]
	}`

	llm := &mockLLM{response: revisionResponse}
	p := NewPlanner(llm, zapNop())

	stepExec := func(_ context.Context, step Step) (string, error) {
		callCount++
		if step.ID == "b" {
			return "", fmt.Errorf("syntax error")
		}
		return "ok", nil
	}

	exec := NewExecutor(p, stepExec, zapNop())

	plan := &Plan{
		ID:   "test",
		Goal: "Test",
		Steps: []Step{
			{ID: "a", Action: "read_file", Status: StepPending},
			{ID: "b", Action: "edit_file", Status: StepPending, DependsOn: []string{"a"}},
		},
		Version: 1,
	}

	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	// The plan should have been revised
	if result.Plan.Version < 2 {
		t.Error("expected plan to be revised")
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && len(substr) > 0 && containsImpl(s, substr))
}

func containsImpl(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func zapNop() *zap.Logger {
	return zap.NewNop()
}

// Verify JSON serialization round-trip
func TestStep_JSONRoundTrip(t *testing.T) {
	step := Step{
		ID:          "step_1",
		Action:      "edit_file",
		Description: "Fix the bug",
		Parameters:  json.RawMessage(`{"path":"main.go"}`),
		DependsOn:   []string{"step_0"},
		Status:      StepPending,
	}

	data, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}

	var restored Step
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if restored.ID != step.ID || restored.Action != step.Action {
		t.Error("round-trip mismatch")
	}
}
