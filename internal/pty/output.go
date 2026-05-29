package pty

import (
	"regexp"
	"strings"
)

var ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// stripANSI removes ANSI escape sequences from text.
func stripANSI(s string) string {
	return ansiEscapeRegex.ReplaceAllString(s, "")
}

// truncateOutput truncates output to maxBytes, adding a truncation marker.
func truncateOutput(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	return s[:maxBytes] + "\n... (output truncated)", true
}

// detectPrompt attempts to detect common shell prompts in output.
// Returns true if the line looks like a prompt.
func detectPrompt(line string) bool {
	// Common prompt patterns
	patterns := []string{
		"$ ",
		"# ",
		"> ",
		">>> ",
		"bash-",
		"sh-",
	}

	trimmed := strings.TrimSpace(line)
	for _, p := range patterns {
		if strings.HasSuffix(trimmed, p) {
			return true
		}
	}
	return false
}
