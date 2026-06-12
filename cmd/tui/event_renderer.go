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
		intent := ev.Intent
		if intent == "" {
			intent = "processing"
		}
		return stepDividerStyle.Render(
			fmt.Sprintf("━━━ Step %d/%d [%s] ━━━", ev.Step, ev.MaxSteps, intent),
		)

	case "thinking":
		content := ev.Content
		if len(content) > 300 {
			content = content[:300] + "..."
		}
		if content == "" {
			return ""
		}
		return thinkingStyle.Render("💭 " + content)

	case "llm_call_started":
		return thinkingStyle.Render("⏳ Calling LLM...")

	case "llm_call_completed":
		return thinkingStyle.Render("✓ LLM call completed")

	case "tool_call":
		toolName := ev.ToolName
		if toolName == "" {
			toolName = "unknown"
		}
		args := ev.ToolArgs
		if len(args) > 150 {
			args = args[:150] + "..."
		}
		if args != "" {
			return toolCallStyle.Render(fmt.Sprintf("🔧 %s(%s)", toolName, args))
		}
		return toolCallStyle.Render(fmt.Sprintf("🔧 %s", toolName))

	case "tool_result":
		content := ev.Content
		if len(content) > 400 {
			content = content[:400] + "\n    ... (truncated)"
		}
		if ev.IsError {
			return toolErrorStyle.Render("❌ FAIL: " + content)
		}
		return toolResultStyle.Render("✓ OK: " + content)

	case "message":
		if ev.Content == "" {
			return ""
		}
		rendered := ev.Content
		if markdownRenderer != nil {
			if md, err := markdownRenderer.Render(ev.Content); err == nil {
				rendered = strings.TrimSpace(md)
			}
		}
		return assistantMsgStyle.Render("🤖 Assistant:\n" + rendered)

	case "rag_context":
		content := ev.Content
		if len(content) > 200 {
			content = content[:200] + "..."
		}
		return thinkingStyle.Render("📚 RAG: " + content)

	case "error":
		return errorMsgStyle.Render("⚠️  Error: " + ev.Content)

	case "done":
		return stepDividerStyle.Render("━━━ Complete ━━━")

	case "session":
		// Session event usually just contains session_id, skip rendering
		return ""

	default:
		// For unknown events, show type only if there's content
		if ev.Content != "" {
			return thinkingStyle.Render(fmt.Sprintf("[%s] %s", ev.Type, ev.Content))
		}
		return ""
	}
}