package main

import (
	"fmt"
	"strings"

	"github.com/agent/code_agent/internal/models"
	"github.com/charmbracelet/glamour"
)

// markdownRenderer is a shared glamour renderer for assistant messages.
var markdownRenderer *glamour.TermRenderer

func init() {
	var err error
	markdownRenderer, err = glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(100),
	)
	if err != nil {
		markdownRenderer = nil
	}
}

// renderEvent converts a ReactStreamEvent to a styled terminal string.
func renderEvent(ev models.ReactStreamEvent) string {
	switch ev.Type {
	case "step_start":
		return stepDividerStyle.Render(
			fmt.Sprintf("--- Step %d/%d [%s] ---", ev.Step, ev.MaxSteps, ev.Intent),
		)

	case "thinking":
		content := ev.Content
		if len(content) > 200 {
			content = content[:200] + "..."
		}
		return thinkingStyle.Render("thinking: " + content)

	case "tool_call":
		args := ev.ToolArgs
		if len(args) > 100 {
			args = args[:100] + "..."
		}
		return toolCallStyle.Render(fmt.Sprintf("tool: %s(%s)", ev.ToolName, args))

	case "tool_result":
		content := ev.Content
		if len(content) > 300 {
			content = content[:300] + "\n    ... (truncated)"
		}
		if ev.IsError {
			return toolErrorStyle.Render("FAIL: " + content)
		}
		return toolResultStyle.Render("OK: " + content)

	case "message":
		rendered := ev.Content
		if markdownRenderer != nil {
			if md, err := markdownRenderer.Render(ev.Content); err == nil {
				rendered = strings.TrimSpace(md)
			}
		}
		return assistantMsgStyle.Render(rendered)

	case "rag_context":
		return thinkingStyle.Render("rag: " + ev.Content)

	case "error":
		return errorMsgStyle.Render("Error: " + ev.Content)

	case "done":
		return stepDividerStyle.Render("--- Complete ---")

	default:
		return thinkingStyle.Render(fmt.Sprintf("[%s] %s", ev.Type, ev.Content))
	}
}