package orchestrator

import (
	"fmt"

	"github.com/agent/code_agent/internal/models"
)

const (
	microPlanTriggerStep = 3
	microPlanInterval    = 6
)

// microPlanPrompt generates a system message asking the LLM to produce a short plan
// before its next action, triggered periodically in multi-step tasks.
func microPlanPrompt(step, maxSteps int) *models.Message {
	if step < microPlanTriggerStep {
		return nil
	}
	if (step-microPlanTriggerStep)%microPlanInterval != 0 {
		return nil
	}

	remaining := maxSteps - step
	content := fmt.Sprintf(
		"[MICRO-PLAN REQUEST — Step %d/%d, %d steps remaining]\n"+
			"Before your next tool call, briefly state:\n"+
			"1. What you've accomplished so far (1 sentence)\n"+
			"2. Your next 2-3 actions and why\n"+
			"3. What would signal you're done\n"+
			"Keep it under 50 words. Then proceed with the next tool call.",
		step, maxSteps, remaining)

	return &models.Message{
		Role:    models.RoleSystem,
		Content: content,
	}
}
