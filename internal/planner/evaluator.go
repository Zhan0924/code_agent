package planner

import (
	"fmt"
	"strings"
)

// PlanQuality represents a multi-dimensional assessment of a plan's quality.
type PlanQuality struct {
	Completeness float64  // 0.0–1.0, does the plan cover all aspects of the goal?
	Feasibility  float64  // 0.0–1.0, are the steps executable with available tools?
	Efficiency   float64  // 0.0–1.0, is the plan minimal without redundant steps?
	Robustness   float64  // 0.0–1.0, does the plan handle potential failures?
	Overall      float64  // weighted average
	Weaknesses   []string // specific issues found
}

// PlanEvaluator assesses plan quality using heuristic rules.
type PlanEvaluator struct {
	validActions map[string]bool
}

// NewPlanEvaluator creates an evaluator with known valid actions.
func NewPlanEvaluator(validActions []string) *PlanEvaluator {
	m := make(map[string]bool)
	for _, a := range validActions {
		m[a] = true
	}
	return &PlanEvaluator{validActions: m}
}

// Evaluate analyzes a plan and returns a quality assessment.
func (e *PlanEvaluator) Evaluate(plan *Plan, goal string) PlanQuality {
	q := PlanQuality{
		Completeness: 1.0,
		Feasibility:  1.0,
		Efficiency:   1.0,
		Robustness:   1.0,
		Weaknesses:   []string{},
	}

	if len(plan.Steps) == 0 {
		q.Completeness = 0.0
		q.Weaknesses = append(q.Weaknesses, "Plan has no steps")
		q.Overall = 0.0
		return q
	}

	e.checkCompleteness(plan, goal, &q)
	e.checkFeasibility(plan, &q)
	e.checkEfficiency(plan, &q)
	e.checkRobustness(plan, &q)

	// Weighted average: completeness and feasibility are most critical
	q.Overall = (q.Completeness*0.35 + q.Feasibility*0.35 + q.Efficiency*0.15 + q.Robustness*0.15)
	return q
}

func (e *PlanEvaluator) checkCompleteness(plan *Plan, goal string, q *PlanQuality) {
	// Heuristic: check if goal keywords appear in step descriptions
	goalLower := strings.ToLower(goal)
	keywords := extractKeywords(goalLower)

	if len(keywords) == 0 {
		return // can't assess without keywords
	}

	covered := 0
	for _, kw := range keywords {
		for _, step := range plan.Steps {
			if strings.Contains(strings.ToLower(step.Description), kw) {
				covered++
				break
			}
		}
	}

	coverage := float64(covered) / float64(len(keywords))
	if coverage < 0.5 {
		q.Completeness = coverage
		q.Weaknesses = append(q.Weaknesses, fmt.Sprintf("Only %.0f%% of goal keywords covered in plan", coverage*100))
	}

	// Check for missing verification step
	hasVerification := false
	for _, step := range plan.Steps {
		if strings.Contains(step.Action, "test") || strings.Contains(strings.ToLower(step.Description), "verify") {
			hasVerification = true
			break
		}
	}
	if !hasVerification && len(plan.Steps) > 2 {
		q.Completeness *= 0.9
		q.Weaknesses = append(q.Weaknesses, "No verification/test step found")
	}
}

func (e *PlanEvaluator) checkFeasibility(plan *Plan, q *PlanQuality) {
	invalidCount := 0
	for _, step := range plan.Steps {
		if !e.validActions[step.Action] && step.Action != "think" {
			invalidCount++
			q.Weaknesses = append(q.Weaknesses, fmt.Sprintf("Unknown action: %s", step.Action))
		}
	}

	if invalidCount > 0 {
		q.Feasibility = 1.0 - (float64(invalidCount) / float64(len(plan.Steps)))
	}

	// Check for circular dependencies (already validated by ValidateDAG, but double-check)
	if err := ValidateDAG(plan.Steps); err != nil {
		q.Feasibility = 0.0
		q.Weaknesses = append(q.Weaknesses, "DAG validation failed: "+err.Error())
	}
}

func (e *PlanEvaluator) checkEfficiency(plan *Plan, q *PlanQuality) {
	// Detect redundant steps: same action + similar description
	seen := make(map[string]int)
	for _, step := range plan.Steps {
		key := step.Action + ":" + normalizeDesc(step.Description)
		seen[key]++
	}

	redundant := 0
	for _, count := range seen {
		if count > 1 {
			redundant += count - 1
		}
	}

	if redundant > 0 {
		q.Efficiency = 1.0 - (float64(redundant) / float64(len(plan.Steps)))
		q.Weaknesses = append(q.Weaknesses, fmt.Sprintf("%d potentially redundant steps", redundant))
	}

	// Penalize overly long plans (>20 steps is suspicious)
	if len(plan.Steps) > 20 {
		q.Efficiency *= 0.8
		q.Weaknesses = append(q.Weaknesses, fmt.Sprintf("Plan has %d steps (may be over-complicated)", len(plan.Steps)))
	}
}

func (e *PlanEvaluator) checkRobustness(plan *Plan, q *PlanQuality) {
	// Check if plan has error handling or fallback steps
	hasErrorHandling := false
	for _, step := range plan.Steps {
		desc := strings.ToLower(step.Description)
		if strings.Contains(desc, "if fail") || strings.Contains(desc, "fallback") || strings.Contains(desc, "retry") {
			hasErrorHandling = true
			break
		}
	}

	// For complex plans (>5 steps), lack of error handling is a weakness
	if !hasErrorHandling && len(plan.Steps) > 5 {
		q.Robustness = 0.7
		q.Weaknesses = append(q.Weaknesses, "No explicit error handling or fallback steps")
	}

	// Check for overly long dependency chains (>5 sequential steps)
	levels, _ := TopologicalSort(plan.Steps)
	if len(levels) > 5 {
		q.Robustness *= 0.9
		q.Weaknesses = append(q.Weaknesses, fmt.Sprintf("Long dependency chain (%d levels) may be fragile", len(levels)))
	}
}

// extractKeywords pulls significant words from a goal string.
func extractKeywords(goal string) []string {
	stopwords := map[string]bool{
		"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
		"in": true, "on": true, "at": true, "to": true, "for": true, "of": true,
		"with": true, "by": true, "from": true, "as": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true, "did": true,
		"will": true, "would": true, "should": true, "could": true, "may": true, "might": true,
		"can": true, "must": true, "shall": true,
	}

	words := strings.FieldsFunc(goal, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})

	var keywords []string
	for _, w := range words {
		w = strings.ToLower(w)
		if len(w) > 2 && !stopwords[w] {
			keywords = append(keywords, w)
		}
	}
	return keywords
}

// normalizeDesc simplifies a description for redundancy detection.
func normalizeDesc(desc string) string {
	desc = strings.ToLower(desc)
	desc = strings.TrimSpace(desc)
	// Keep first 50 chars as fingerprint
	if len(desc) > 50 {
		desc = desc[:50]
	}
	return desc
}

// ShouldImprove returns true if the plan quality is below acceptable threshold.
func (q *PlanQuality) ShouldImprove() bool {
	return q.Overall < 0.7
}

// FormatReport generates a human-readable quality report.
func (q *PlanQuality) FormatReport() string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Plan Quality: %.0f%%", q.Overall*100))
	lines = append(lines, fmt.Sprintf("  Completeness: %.0f%%", q.Completeness*100))
	lines = append(lines, fmt.Sprintf("  Feasibility:  %.0f%%", q.Feasibility*100))
	lines = append(lines, fmt.Sprintf("  Efficiency:   %.0f%%", q.Efficiency*100))
	lines = append(lines, fmt.Sprintf("  Robustness:   %.0f%%", q.Robustness*100))

	if len(q.Weaknesses) > 0 {
		lines = append(lines, "\nWeaknesses:")
		for _, w := range q.Weaknesses {
			lines = append(lines, "  - "+w)
		}
	}

	return strings.Join(lines, "\n")
}
