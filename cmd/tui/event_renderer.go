package main

import (
	"fmt"
	"strings"

	"github.com/agent/code_agent/internal/models"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
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

// renderStepStart renders step start event
func renderStepStart(ev models.ReactStreamEvent) string {
	intent := ev.Intent
	if intent == "" {
		intent = "processing"
	}
	return stepDividerStyle.Render(
		fmt.Sprintf("━━━ Step %d/%d [%s] ━━━", ev.Step, ev.MaxSteps, intent),
	)
}

// renderThinking renders thinking content
func renderThinking(content string, expanded bool) string {
	if !expanded {
		return collapsedStyle.Render("💭 Thinking...")
	}
	if len(content) > 300 {
		content = content[:300] + "..."
	}
	if content == "" {
		return ""
	}
	return thinkingStyle.Render("💭 " + content)
}

// renderToolCall renders tool call event
func renderToolCall(ev models.ReactStreamEvent, expanded bool) string {
	toolName := ev.ToolName
	if toolName == "" {
		toolName = "unknown"
	}
	
	if !expanded {
		return collapsedStyle.Render(fmt.Sprintf("🔧 %s(...)", toolName))
	}
	
	args := ev.ToolArgs
	if len(args) > 150 {
		args = args[:150] + "..."
	}
	if args != "" {
		return toolCallStyle.Render(fmt.Sprintf("🔧 %s(%s)", toolName, args))
	}
	return toolCallStyle.Render(fmt.Sprintf("🔧 %s", toolName))
}

// renderToolResult renders tool result
func renderToolResult(ev models.ReactStreamEvent, expanded bool) string {
	if !expanded {
		if ev.IsError {
			return collapsedStyle.Render("❌ Tool failed")
		}
		return collapsedStyle.Render("✓ Tool success")
	}
	
	content := ev.Content
	if len(content) > 400 {
		content = content[:400] + "\n    ... (truncated)"
	}
	if ev.IsError {
		return toolErrorStyle.Render("❌ FAIL: " + content)
	}
	return toolResultStyle.Render("✓ OK: " + content)
}

// renderMessage renders assistant message with markdown
func renderMessage(content string, expanded bool) string {
	if content == "" {
		return ""
	}
	
	rendered := content
	if markdownRenderer != nil {
		if md, err := markdownRenderer.Render(content); err == nil {
			rendered = strings.TrimSpace(md)
		}
	}
	
	// Add assistant header
	header := lipgloss.NewStyle().
		Foreground(successColor).
		Bold(true).
		Render("🤖 Assistant")
	
	return lipgloss.NewStyle().
		BorderLeft(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(successColor).
		Padding(0, 1).
		MarginLeft(1).
		Render(header + "\n\n" + rendered)
}

// renderEvent converts a ReactStreamEvent to a styled terminal string (legacy function)
func renderEvent(ev models.ReactStreamEvent) string {
	switch ev.Type {
	case "step_start":
		return renderStepStart(ev)

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
		return renderToolCall(ev, true)

	case "tool_result":
		return renderToolResult(ev, true)

	case "message":
		return renderMessage(ev.Content, true)

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