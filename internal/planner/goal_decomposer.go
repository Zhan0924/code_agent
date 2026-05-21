package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// SubGoal represents a decomposed sub-goal with its own plan.
type SubGoal struct {
	ID          string `json:"id"`
	ParentID    string `json:"parent_id,omitempty"`
	Description string `json:"description"`
	Priority    int    `json:"priority"`
	Plan        *Plan  `json:"plan,omitempty"`
	Status      StepStatus `json:"status"`
}

// GoalDecomposer breaks high-level goals into manageable sub-goals.
type GoalDecomposer struct {
	llm    LLMCaller
	logger *zap.Logger
}

// NewGoalDecomposer creates a goal decomposer.
func NewGoalDecomposer(llm LLMCaller, logger *zap.Logger) *GoalDecomposer {
	return &GoalDecomposer{
		llm:    llm,
		logger: logger.With(zap.String("component", "planner.goal_decomposer")),
	}
}

const decomposerSystemPrompt = `You are a goal decomposition agent. Given a complex high-level goal, break it into 2-4 independent sub-goals that can each be planned and executed separately.

Rules:
1. Each sub-goal should be self-contained and achievable independently (or with minimal dependencies).
2. Sub-goals should be ordered by priority (most critical first).
3. Each sub-goal description should be specific enough to generate a detailed plan from.
4. Output ONLY valid JSON matching this schema:
{
  "sub_goals": [
    {"id": "sg_1", "description": "...", "priority": 1, "depends_on": []},
    {"id": "sg_2", "description": "...", "priority": 2, "depends_on": ["sg_1"]}
  ],
  "reasoning": "why this decomposition"
}`

// Decompose breaks a complex goal into sub-goals using LLM.
func (d *GoalDecomposer) Decompose(ctx context.Context, goal string, contextInfo string) ([]SubGoal, error) {
	userPrompt := fmt.Sprintf("Goal: %s", goal)
	if contextInfo != "" {
		userPrompt += fmt.Sprintf("\n\nContext:\n%s", contextInfo)
	}

	raw, err := d.llm.Call(ctx, decomposerSystemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("goal decomposition LLM call failed: %w", err)
	}

	subGoals, err := parseSubGoals(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to parse sub-goals: %w", err)
	}

	for i := range subGoals {
		if subGoals[i].ID == "" {
			subGoals[i].ID = fmt.Sprintf("sg_%d_%d", time.Now().UnixNano(), i)
		}
		subGoals[i].Status = StepPending
	}

	d.logger.Info("goal decomposed",
		zap.String("goal", goal),
		zap.Int("sub_goals", len(subGoals)))

	return subGoals, nil
}

func parseSubGoals(raw string) ([]SubGoal, error) {
	cleaned := extractJSON(raw)

	var result struct {
		SubGoals []SubGoal `json:"sub_goals"`
	}
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w (raw: %.200s)", err, raw)
	}
	return result.SubGoals, nil
}

func extractJSON(raw string) string {
	// Reuse the same logic from parsePlanJSON
	cleaned := raw
	if idx := indexOf(cleaned, "```json"); idx >= 0 {
		cleaned = cleaned[idx+7:]
	} else if idx := indexOf(cleaned, "```"); idx >= 0 {
		cleaned = cleaned[idx+3:]
	}
	if idx := lastIndexOf(cleaned, "```"); idx >= 0 {
		cleaned = cleaned[:idx]
	}
	return trimSpace(cleaned)
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func lastIndexOf(s, substr string) int {
	last := -1
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			last = i
		}
	}
	return last
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
