package main

import "github.com/charmbracelet/lipgloss"

var (
	// Colors - Modern Nord-inspired palette
	primaryColor    = lipgloss.Color("#88C0D0") // Nord frost - cyan
	secondaryColor  = lipgloss.Color("#81A1C1") // Nord frost - blue
	accentColor     = lipgloss.Color("#B48EAD") // Nord aurora - purple
	successColor    = lipgloss.Color("#A3BE8C") // Nord aurora - green
	warningColor    = lipgloss.Color("#EBCB8B") // Nord aurora - yellow
	errorColor      = lipgloss.Color("#BF616A") // Nord aurora - red
	mutedColor      = lipgloss.Color("#4C566A") // Nord polar - gray
	textColor       = lipgloss.Color("#ECEFF4") // Nord snow - white
	bgColor         = lipgloss.Color("#2E3440") // Nord polar - dark
	bgHighlightColor = lipgloss.Color("#3B4252") // Nord polar - lighter

	// Title
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(primaryColor).
			Background(bgColor).
			Padding(0, 2).
			MarginBottom(1)

	// User message
	userMsgStyle = lipgloss.NewStyle().
			Foreground(textColor).
			Bold(true).
			Padding(0, 2).
			BorderLeft(true).
			BorderStyle(lipgloss.ThickBorder()).
			BorderForeground(primaryColor).
			BorderBackground(bgColor).
			MarginLeft(1)

	// Thinking
	thinkingStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			Italic(true).
			PaddingLeft(3).
			Foreground(lipgloss.Color("#D8DEE9"))

	// Tool call
	toolCallStyle = lipgloss.NewStyle().
			Foreground(secondaryColor).
			Bold(true).
			PaddingLeft(2).
			Background(bgHighlightColor).
			Padding(0, 2).
			MarginLeft(1)

	// Tool result
	toolResultStyle = lipgloss.NewStyle().
			Foreground(successColor).
			PaddingLeft(4)

	// Tool error
	toolErrorStyle = lipgloss.NewStyle().
			Foreground(errorColor).
			Bold(true).
			PaddingLeft(4)

	// Assistant message
	assistantMsgStyle = lipgloss.NewStyle().
				Foreground(textColor).
				Padding(0, 2).
				BorderLeft(true).
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(successColor).
				MarginLeft(1)

	// Error message
	errorMsgStyle = lipgloss.NewStyle().
			Foreground(errorColor).
			Bold(true).
			PaddingLeft(2).
			Background(lipgloss.Color("#434C5E")).
			Padding(0, 2).
			MarginLeft(1)

	// Step divider
	stepDividerStyle = lipgloss.NewStyle().
				Foreground(secondaryColor).
				PaddingLeft(2).
				Bold(true)

	// Status bar base
	statusBarStyle = lipgloss.NewStyle().
			Background(bgHighlightColor).
			Foreground(textColor).
			Padding(0, 1)

	// Status bar states
	statusBarIdleStyle = lipgloss.NewStyle().
				Foreground(mutedColor).
				Background(bgHighlightColor).
				Padding(0, 1)

	statusBarActiveStyle = lipgloss.NewStyle().
				Foreground(primaryColor).
				Background(bgHighlightColor).
				Bold(true).
				Padding(0, 1)

	statusBarToolStyle = lipgloss.NewStyle().
				Foreground(accentColor).
				Background(bgHighlightColor).
				Bold(true).
				Padding(0, 1)

	statusBarModelStyle = lipgloss.NewStyle().
				Foreground(secondaryColor).
				Background(bgHighlightColor).
				Padding(0, 1)

	statusBarBranchStyle = lipgloss.NewStyle().
				Foreground(accentColor).
				Background(bgHighlightColor).
				Padding(0, 1)

	statusBarStepStyle = lipgloss.NewStyle().
				Foreground(warningColor).
				Background(bgHighlightColor).
				Bold(true).
				Padding(0, 1)

	// Header panel
	headerStyle = lipgloss.NewStyle().
			Background(bgColor).
			Foreground(primaryColor).
			Padding(0, 2).
			Bold(true)

	// Help text
	helpStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			Italic(true).
			PaddingLeft(2)
)