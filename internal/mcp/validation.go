package mcp

import (
	"fmt"
	"path/filepath"
	"strings"
)

var AllowedMCPCommands = map[string]bool{
	"npx":    true,
	"node":   true,
	"python": true,
	"python3": true,
	"uvx":    true,
	"uv":     true,
	"deno":   true,
	"bun":    true,
	"docker": true,
}

var allowedCommandDirs = []string{
	"/usr/bin/",
	"/usr/local/bin/",
	"/opt/homebrew/bin/",
}

var dangerousArgs = []string{"--eval", "-e", "-c", "eval", "exec"}

func ValidateCommand(cmd string) error {
	if cmd == "" {
		return fmt.Errorf("command must not be empty")
	}

	// Reject relative paths with directory separators (e.g. "../bin/evil", "./foo")
	if !filepath.IsAbs(cmd) && strings.ContainsRune(cmd, filepath.Separator) {
		return fmt.Errorf("relative paths with directory separators not allowed: %s", cmd)
	}

	// Clean the path to resolve ".." tricks before checking
	cleaned := filepath.Clean(cmd)
	base := filepath.Base(cleaned)
	if !AllowedMCPCommands[base] {
		return fmt.Errorf("command not in whitelist: %s (allowed: npx, node, python, python3, uvx, uv, deno, bun, docker)", base)
	}

	if filepath.IsAbs(cleaned) {
		allowed := false
		for _, dir := range allowedCommandDirs {
			if strings.HasPrefix(cleaned, dir) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("absolute command path not in allowed directories: %s", cleaned)
		}
	}

	return nil
}

func ValidateArgs(args []string) error {
	for _, arg := range args {
		for _, dangerous := range dangerousArgs {
			if arg == dangerous || strings.HasPrefix(arg, dangerous+"=") {
				return fmt.Errorf("dangerous argument not allowed: %s", arg)
			}
		}
	}
	return nil
}
