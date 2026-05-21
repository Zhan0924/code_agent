package planner

import (
	"testing"
)

func TestPlanEvaluator_EmptyPlan(t *testing.T) {
	e := NewPlanEvaluator([]string{"read_file", "write_file"})
	plan := &Plan{Goal: "test", Steps: []Step{}}

	q := e.Evaluate(plan, "test")
	if q.Overall != 0.0 {
		t.Errorf("expected 0.0 for empty plan, got %f", q.Overall)
	}
	if !q.ShouldImprove() {
		t.Error("empty plan should need improvement")
	}
}

func TestPlanEvaluator_GoodPlan(t *testing.T) {
	e := NewPlanEvaluator([]string{"read_file", "edit_file", "run_tests"})
	plan := &Plan{
		Goal: "fix the authentication bug in login handler",
		Steps: []Step{
			{ID: "step_1", Action: "read_file", Description: "Read the login handler to understand the authentication flow"},
			{ID: "step_2", Action: "edit_file", Description: "Fix the bug in authentication logic", DependsOn: []string{"step_1"}},
			{ID: "step_3", Action: "run_tests", Description: "Run tests to verify the fix works", DependsOn: []string{"step_2"}},
		},
	}

	q := e.Evaluate(plan, plan.Goal)
	if q.Overall < 0.7 {
		t.Errorf("expected good plan to score >= 0.7, got %f\nWeaknesses: %v", q.Overall, q.Weaknesses)
	}
	if q.ShouldImprove() {
		t.Errorf("good plan should not need improvement, score=%f", q.Overall)
	}
}

func TestPlanEvaluator_UnknownActions(t *testing.T) {
	e := NewPlanEvaluator([]string{"read_file", "write_file"})
	plan := &Plan{
		Goal: "deploy the app",
		Steps: []Step{
			{ID: "step_1", Action: "read_file", Description: "Read config"},
			{ID: "step_2", Action: "deploy_to_prod", Description: "Deploy to production", DependsOn: []string{"step_1"}},
			{ID: "step_3", Action: "notify_slack", Description: "Notify team", DependsOn: []string{"step_2"}},
		},
	}

	q := e.Evaluate(plan, plan.Goal)
	if q.Feasibility >= 1.0 {
		t.Errorf("expected feasibility < 1.0 with unknown actions, got %f", q.Feasibility)
	}
	if len(q.Weaknesses) == 0 {
		t.Error("expected weaknesses for unknown actions")
	}
}

func TestPlanEvaluator_RedundantSteps(t *testing.T) {
	e := NewPlanEvaluator([]string{"read_file", "edit_file"})
	plan := &Plan{
		Goal: "update config",
		Steps: []Step{
			{ID: "step_1", Action: "read_file", Description: "Read the config file"},
			{ID: "step_2", Action: "read_file", Description: "Read the config file"},
			{ID: "step_3", Action: "edit_file", Description: "Edit config", DependsOn: []string{"step_1", "step_2"}},
		},
	}

	q := e.Evaluate(plan, plan.Goal)
	if q.Efficiency >= 1.0 {
		t.Errorf("expected efficiency < 1.0 with redundant steps, got %f", q.Efficiency)
	}
}

func TestPlanQuality_FormatReport(t *testing.T) {
	q := PlanQuality{
		Completeness: 0.8,
		Feasibility:  0.9,
		Efficiency:   0.7,
		Robustness:   0.6,
		Overall:      0.78,
		Weaknesses:   []string{"No verification step", "Long dependency chain"},
	}

	report := q.FormatReport()
	if report == "" {
		t.Error("expected non-empty report")
	}
}
