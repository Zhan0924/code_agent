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
	LineStart int // Starting line in rendered output (for click detection)
	LineEnd   int // Ending line in rendered output
}

// MessageModel manages the display state of chat messages
type MessageModel struct {
	blocks       []MessageBlock
	lastFinalIdx int // index of the last final message for copy
	clickedBlock int // index of clicked block (-1 if none)
}

func newMessageModel() *MessageModel {
	return &MessageModel{
		blocks:       []MessageBlock{},
		lastFinalIdx: -1,
		clickedBlock: -1,
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

func (m *MessageModel) HandleClick(y int) int {
	// Find which block was clicked based on line number
	for i, block := range m.blocks {
		if y >= block.LineStart && y <= block.LineEnd {
			if !block.IsFinal {
				m.ToggleExpand(i)
				return i
			}
		}
	}
	return -1
}

func (m *MessageModel) Render(width int) string {
	var lines []string
	currentLine := 0
	
	for i, block := range m.blocks {
		m.blocks[i].LineStart = currentLine
		
		if block.IsFinal {
			// Final messages are always expanded
			blockLines := strings.Split(block.Content, "\n")
			lines = append(lines, blockLines...)
			currentLine += len(blockLines)
		} else {
			// Intermediate steps can be collapsed
			if block.Expanded {
				blockLines := strings.Split(block.Content, "\n")
				lines = append(lines, blockLines...)
				currentLine += len(blockLines)
			} else {
				// Show collapsed preview with click indicator
				preview := m.renderCollapsed(block, i)
				lines = append(lines, preview)
				currentLine++
			}
		}
		
		m.blocks[i].LineEnd = currentLine - 1
		
		// Add spacing
		lines = append(lines, "")
		currentLine++
	}
	
	return strings.Join(lines, "\n")
}

func (m *MessageModel) renderCollapsed(block MessageBlock, idx int) string {
	// Show a collapsed line with click hint
	var preview string
	switch block.Type {
	case "thinking":
		preview = fmt.Sprintf("💭 Thinking %s", collapsedStyle.Render("[click to expand]"))
	case "llm_call_started":
		preview = "⏳ Calling LLM..."
	case "llm_call_completed":
		preview = "✓ LLM call completed"
	case "tool_call":
		toolName := "unknown"
		if name, ok := block.Metadata["tool_name"].(string); ok {
			toolName = name
		}
		preview = fmt.Sprintf("🔧 %s(...) %s", toolName, collapsedStyle.Render("[click to expand]"))
	case "tool_result":
		if isErr, ok := block.Metadata["is_error"].(bool); ok && isErr {
			preview = "❌ Tool failed"
		} else {
			preview = "✓ Tool success"
		}
	case "step_start":
		return block.Content // Steps are always visible
	default:
		preview = fmt.Sprintf("... %s", collapsedStyle.Render("[click to expand]"))
	}
	
	return collapsedBoxStyle.Render(preview)
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
	m.clickedBlock = -1
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