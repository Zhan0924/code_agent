package toollearn

import (
	"fmt"

	"go.uber.org/zap"
)

// Advisor provides pre-dispatch recommendations based on learned patterns.
type Advisor struct {
	extractor *Extractor
	logger    *zap.Logger
}

// NewAdvisor creates an advisor backed by a pattern extractor.
func NewAdvisor(extractor *Extractor, logger *zap.Logger) *Advisor {
	return &Advisor{
		extractor: extractor,
		logger:    logger.With(zap.String("component", "toollearn.advisor")),
	}
}

// Advise returns an Advice for the given tool call, or nil if no advice is warranted.
func (a *Advisor) Advise(toolName string) *Advice {
	pattern := a.extractor.GetPattern(toolName)
	if pattern == nil || pattern.SampleSize < 5 {
		return nil
	}

	advice := &Advice{ToolName: toolName}

	if pattern.FailureRate > 0.5 {
		advice.Warning = fmt.Sprintf(
			"Tool %q has a %.0f%% failure rate (last %d calls). Common errors: %v",
			toolName, pattern.FailureRate*100, pattern.SampleSize, pattern.CommonErrors,
		)
	}

	if pattern.AvgDuration > 10000 {
		advice.Hint = fmt.Sprintf(
			"Tool %q averages %dms — consider whether a faster alternative exists.",
			toolName, pattern.AvgDuration,
		)
	}

	if advice.Warning == "" && advice.Hint == "" {
		return nil
	}
	return advice
}

// FormatForLLM renders advice as a system-level hint for the LLM.
func (a *Advisor) FormatForLLM(toolName string) string {
	adv := a.Advise(toolName)
	if adv == nil {
		return ""
	}
	var msg string
	if adv.Warning != "" {
		msg += "[Tool Learning Warning] " + adv.Warning
	}
	if adv.Hint != "" {
		if msg != "" {
			msg += " "
		}
		msg += "[Hint] " + adv.Hint
	}
	return msg
}
