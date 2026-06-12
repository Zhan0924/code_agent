package main

import (
	"fmt"
	"strings"

	"github.com/agent/code_agent/internal/models"
)

// MessageBlock represents a renderable message block in the chat
type MessageBlock struct {
	Type      string // "thinking", "tool_call", "tool_result", "message", "error", "step"
	Content   string
	Expanded  bool
	IsFinal   bool // true for final output (message), false for intermediate
	Metadata  map[string]interface{}
}

// MessageModel manages the display state of chat messages
type MessageModel struct {
	blocks      []MessageBlock
	lastFinalIdx int // index of the last final message for copy
}

func newMessageModel() *MessageModel {
	return &MessageModel{
		blocks:      []MessageBlock{},
		lastFinalIdx: -1,
	}
}

func (m *MessageModel) AddEvent(ev models.ReactStreamEvent) {
	block := MessageBlock{
		Type:     ev.Type,
		Expanded: false,
		Metadata: make(map[string]interface{}),
	}

	switch ev.Type {
	case "step_start":
		block.Content = renderStepStart(ev)
		block.IsFinal = false
		
	case "thinking":
		if ev.Content != "" {
			block.Content = renderThinking(ev.Content, false)
			block.IsFinal = false
		}
		
	case "llm_call_started":
		block.Content = thinkingStyle.Render("⏳ Calling LLM...")
		block.IsFinal = false
		
	case "llm_call_completed":
		block.Content = thinkingStyle.Render("✓ LLM call completed")
		block.IsFinal = false
		
	case "tool_call":
		block.Content = renderToolCall(ev, false)
		block.IsFinal = false
		block.Metadata["tool_name"] = ev.ToolName
		block.Metadata["tool_args"] = ev.ToolArgs
		
	case "tool_result":
		block.Content = renderToolResult(ev, false)
		block.IsFinal = false
		
	case "message":
		block.Content = renderMessage(ev.Content, true)
		block.IsFinal = true
		m.lastFinalIdx = len(m.blocks)
		
	case "error":
		block.Content = errorMsgStyle.Render("⚠️  Error: " + ev.Content)
		block.IsFinal = false
		
	case "done":
		block.Content = stepDividerStyle.Render("━━━ Complete ━━━")
		block.IsFinal = false
		
	case "session":
		// Skip session events
		return
		
	default:
		if ev.Content != "" {
			block.Content = thinkingStyle.Render("["+ev.Type+"] " + ev.Content)
			block.IsFinal = false
		}
	}

	if block.Content != "" {
		m.blocks = append(m.blocks, block)
	}
}

func (m *MessageModel) ToggleExpand(idx int) {
	if idx >= 0 && idx < len(m.blocks) {
		m.blocks[idx].Expanded = !m.blocks[idx].Expanded
	}
}

func (m *MessageModel) Render(width int) string {
	var lines []string
	
	for i, block := range m.blocks {
		if block.IsFinal {
			// Final messages are always expanded
			lines = append(lines, block.Content)
		} else {
			// Intermediate steps can be collapsed
			if block.Expanded {
				lines = append(lines, block.Content)
			} else {
				// Show collapsed preview
				preview := m.renderCollapsed(block, i)
				lines = append(lines, preview)
			}
		}
		lines = append(lines, "") // Add spacing
	}
	
	return strings.Join(lines, "\n")
}

func (m *MessageModel) renderCollapsed(block MessageBlock, idx int) string {
	// Show a collapsed line with toggle hint
	switch block.Type {
	case "thinking":
		return collapsedStyle.Render("💭 Thinking... [Press 'e' to expand]")
	case "tool_call":
		toolName := "unknown"
		if name, ok := block.Metadata["tool_name"].(string); ok {
			toolName = name
		}
		return collapsedStyle.Render(fmt.Sprintf("🔧 Tool: %s(...) [Press 'e' to expand]", toolName))
	case "step_start":
		return block.Content // Steps are always visible
	default:
		return collapsedStyle.Render("[...] [Press 'e' to expand]")
	}
}

func (m *MessageModel) GetFinalOutput() string {
	// Collect all final messages (assistant messages)
	var outputs []string
	for _, block := range m.blocks {
		if block.IsFinal && block.Type == "message" {
			// Strip ANSI codes for plain text copy
			outputs = append(outputs, stripANSI(block.Content))
		}
	}
	return strings.Join(outputs, "\n\n")
}

func (m *MessageModel) Clear() {
	m.blocks = []MessageBlock{}
	m.lastFinalIdx = -1
}

// stripANSI removes ANSI escape codes from a string
func stripANSI(s string) string {
	// Simple implementation - remove common ANSI codes
	var result strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		result.WriteRune(r)
	}
	return result.String()
}