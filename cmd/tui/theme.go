package main

import "github.com/charmbracelet/lipgloss"

var (
	// Colors
	primaryColor   = lipgloss.Color("#7C3AED") // purple
	secondaryColor = lipgloss.Color("#06B6D4") // cyan
	successColor   = lipgloss.Color("#10B981") // green
	errorColor     = lipgloss.Color("#EF4444") // red
	mutedColor     = lipgloss.Color("#6B7280") // gray

	// Styles
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(primaryColor).
			MarginBottom(1)

	userMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F9FAFB")).
			Bold(true).
			PaddingLeft(2).
			BorderLeft(true).
			BorderStyle(lipgloss.ThickBorder()).
			BorderForeground(primaryColor)

	thinkingStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			Italic(true).
			PaddingLeft(4)

	toolCallStyle = lipgloss.NewStyle().
			Foreground(secondaryColor).
			Bold(true).
			PaddingLeft(4)

	toolResultStyle = lipgloss.NewStyle().
			Foreground(successColor).
			PaddingLeft(6)

	toolErrorStyle = lipgloss.NewStyle().
			Foreground(errorColor).
			PaddingLeft(6)

	assistantMsgStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#F9FAFB")).
				PaddingLeft(2).
				BorderLeft(true).
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(successColor)

	errorMsgStyle = lipgloss.NewStyle().
			Foreground(errorColor).
			Bold(true).
			PaddingLeft(2)

	statusBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#374151")).
			Foreground(lipgloss.Color("#D1D5DB")).
			Padding(0, 1)

	stepDividerStyle = lipgloss.NewStyle().
				Foreground(mutedColor).
				PaddingLeft(2)
)