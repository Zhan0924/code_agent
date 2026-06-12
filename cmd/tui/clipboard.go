package main

import (
	"os/exec"
)

// copyToClipboard copies text to system clipboard
func copyToClipboard(text string) error {
	// Try pbcopy (macOS)
	cmd := exec.Command("pbcopy")
	stdin, err := cmd.StdinPipe()
	if err == nil {
		go func() {
			defer stdin.Close()
			stdin.Write([]byte(text))
		}()
		if cmd.Run() == nil {
			return nil
		}
	}

	// Try xclip (Linux)
	cmd = exec.Command("xclip", "-selection", "clipboard")
	stdin, err = cmd.StdinPipe()
	if err == nil {
		go func() {
			defer stdin.Close()
			stdin.Write([]byte(text))
		}()
		if cmd.Run() == nil {
			return nil
		}
	}

	// Try xsel (Linux alternative)
	cmd = exec.Command("xsel", "--clipboard", "--input")
	stdin, err = cmd.StdinPipe()
	if err == nil {
		go func() {
			defer stdin.Close()
			stdin.Write([]byte(text))
		}()
		if cmd.Run() == nil {
			return nil
		}
	}

	return nil // Silently fail if no clipboard tool available
}