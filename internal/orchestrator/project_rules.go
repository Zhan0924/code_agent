// project_rules.go loads project-level rule files (.coderules, AGENTS.md,
// .clinerules, CLAUDE.md, etc.) and injects them into the LLM system prompt.
// This allows each project to customize Agent behavior — similar to
// Cursor's .cursorrules, Cline's .clinerules, and Claude Code's CLAUDE.md.
package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ─── Rule File Discovery ────────────────────────────────────────────────────

// ruleFileNames is the ordered list of project rule files to search for.
// The first one found takes priority (but all found are merged).
var ruleFileNames = []string{
	".coderules",                      // Our canonical name
	"AGENTS.md",                       // OpenAI Codex / multi-agent convention
	"CLAUDE.md",                       // Claude Code convention
	".clinerules",                     // Cline convention
	".cursorrules",                    // Cursor convention
	".github/copilot-instructions.md", // GitHub Copilot
	"CODING_GUIDELINES.md",
}

// ProjectRules holds loaded project-level rules for a workspace.
type ProjectRules struct {
	Rules    []RuleFile `json:"rules"`
	Combined string     `json:"combined"` // All rules merged into one string
	LoadedAt time.Time  `json:"loaded_at"`
}

// RuleFile represents a single rule file found in the project.
type RuleFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Source  string `json:"source"` // e.g. ".coderules", "AGENTS.md"
}

// ─── Rule Loader ────────────────────────────────────────────────────────────

// RuleLoader discovers and loads project rule files.
type RuleLoader struct {
	logger *zap.Logger

	mu    sync.RWMutex
	cache map[string]*ProjectRules // rootDir → cached rules
}

// NewRuleLoader creates a new project rule loader.
func NewRuleLoader(logger *zap.Logger) *RuleLoader {
	return &RuleLoader{
		logger: logger,
		cache:  make(map[string]*ProjectRules),
	}
}

// Load discovers and loads all project rule files from the given directory.
// Results are cached per directory (call Invalidate to refresh).
func (rl *RuleLoader) Load(rootDir string) *ProjectRules {
	rl.mu.RLock()
	if cached, ok := rl.cache[rootDir]; ok {
		rl.mu.RUnlock()
		return cached
	}
	rl.mu.RUnlock()

	rules := rl.discover(rootDir)

	rl.mu.Lock()
	rl.cache[rootDir] = rules
	rl.mu.Unlock()

	return rules
}

// Invalidate clears cached rules for a directory (call when files change).
func (rl *RuleLoader) Invalidate(rootDir string) {
	rl.mu.Lock()
	delete(rl.cache, rootDir)
	rl.mu.Unlock()
}

// discover scans the directory for rule files and loads them.
func (rl *RuleLoader) discover(rootDir string) *ProjectRules {
	var rules []RuleFile
	var combined strings.Builder

	for _, name := range ruleFileNames {
		path := filepath.Join(rootDir, name)
		content, err := os.ReadFile(path)
		if err != nil {
			continue // file not found, skip
		}

		text := strings.TrimSpace(string(content))
		if text == "" {
			continue
		}

		// Limit individual rule file to 4KB to prevent token explosion
		if len(text) > 4096 {
			text = text[:4096] + "\n... (truncated)"
		}

		rules = append(rules, RuleFile{
			Path:    name,
			Content: text,
			Source:  name,
		})

		combined.WriteString(fmt.Sprintf("### Rules from %s:\n%s\n\n", name, text))
		rl.logger.Info("project rules loaded",
			zap.String("file", name),
			zap.String("dir", rootDir),
			zap.Int("bytes", len(text)),
		)
	}

	// Also check for language-specific rule files
	langRules := rl.discoverLanguageRules(rootDir)
	for _, lr := range langRules {
		rules = append(rules, lr)
		combined.WriteString(fmt.Sprintf("### %s:\n%s\n\n", lr.Source, lr.Content))
	}

	return &ProjectRules{
		Rules:    rules,
		Combined: combined.String(),
		LoadedAt: time.Now(),
	}
}

// discoverLanguageRules looks for language-specific configuration that hints
// at project conventions (e.g., .golangci.yml, pyproject.toml, tsconfig.json).
func (rl *RuleLoader) discoverLanguageRules(rootDir string) []RuleFile {
	var rules []RuleFile

	// Go: extract key linting rules from .golangci.yml
	if content, err := os.ReadFile(filepath.Join(rootDir, ".golangci.yml")); err == nil {
		summary := extractGolangciSummary(string(content))
		if summary != "" {
			rules = append(rules, RuleFile{
				Path:    ".golangci.yml",
				Content: summary,
				Source:  "Go linting rules (.golangci.yml)",
			})
		}
	}

	// Python: extract from pyproject.toml [tool.ruff] or [tool.black]
	if content, err := os.ReadFile(filepath.Join(rootDir, "pyproject.toml")); err == nil {
		summary := extractPythonToolSummary(string(content))
		if summary != "" {
			rules = append(rules, RuleFile{
				Path:    "pyproject.toml",
				Content: summary,
				Source:  "Python project conventions (pyproject.toml)",
			})
		}
	}

	// TypeScript/JavaScript: tsconfig strictness
	if content, err := os.ReadFile(filepath.Join(rootDir, "tsconfig.json")); err == nil {
		summary := extractTSConfigSummary(string(content))
		if summary != "" {
			rules = append(rules, RuleFile{
				Path:    "tsconfig.json",
				Content: summary,
				Source:  "TypeScript config (tsconfig.json)",
			})
		}
	}

	return rules
}

// ─── Config Summarizers ─────────────────────────────────────────────────────

// extractGolangciSummary extracts key linting settings from .golangci.yml.
func extractGolangciSummary(content string) string {
	if len(content) == 0 {
		return ""
	}
	var hints []string

	if strings.Contains(content, "errcheck") {
		hints = append(hints, "errcheck enabled (handle all errors)")
	}
	if strings.Contains(content, "govet") {
		hints = append(hints, "go vet enabled")
	}
	if strings.Contains(content, "gofmt") || strings.Contains(content, "goimports") {
		hints = append(hints, "strict formatting (gofmt/goimports)")
	}
	if strings.Contains(content, "staticcheck") {
		hints = append(hints, "staticcheck enabled")
	}
	if strings.Contains(content, "gosec") {
		hints = append(hints, "security linting (gosec)")
	}
	if strings.Contains(content, "funlen") {
		hints = append(hints, "function length limits")
	}
	if strings.Contains(content, "cyclop") || strings.Contains(content, "gocyclo") {
		hints = append(hints, "cyclomatic complexity checks")
	}

	if len(hints) == 0 {
		return ""
	}
	return "Go project uses these linting rules: " + strings.Join(hints, ", ") + "."
}

// extractPythonToolSummary extracts key settings from pyproject.toml.
func extractPythonToolSummary(content string) string {
	if len(content) == 0 {
		return ""
	}
	var hints []string

	if strings.Contains(content, "[tool.ruff]") {
		hints = append(hints, "ruff linter configured")
	}
	if strings.Contains(content, "[tool.black]") {
		hints = append(hints, "black formatter configured")
	}
	if strings.Contains(content, "[tool.mypy]") {
		hints = append(hints, "mypy type checking enabled")
	}
	if strings.Contains(content, "[tool.pytest]") || strings.Contains(content, "[tool.pytest.ini_options]") {
		hints = append(hints, "pytest configured")
	}
	if strings.Contains(content, "line-length") {
		// Try to extract line length
		for _, line := range strings.Split(content, "\n") {
			if strings.Contains(line, "line-length") && strings.Contains(line, "=") {
				hints = append(hints, strings.TrimSpace(line))
				break
			}
		}
	}

	if len(hints) == 0 {
		return ""
	}
	return "Python project conventions: " + strings.Join(hints, ", ") + "."
}

// extractTSConfigSummary extracts key TypeScript settings.
func extractTSConfigSummary(content string) string {
	if len(content) == 0 {
		return ""
	}
	var hints []string

	if strings.Contains(content, `"strict": true`) || strings.Contains(content, `"strict":true`) {
		hints = append(hints, "strict mode enabled")
	}
	if strings.Contains(content, `"noImplicitAny": true`) {
		hints = append(hints, "no implicit any")
	}
	if strings.Contains(content, `"strictNullChecks": true`) {
		hints = append(hints, "strict null checks")
	}
	if strings.Contains(content, `"jsx"`) {
		hints = append(hints, "JSX/React support")
	}
	if strings.Contains(content, `"module": "esnext"`) || strings.Contains(content, `"module": "es2022"`) {
		hints = append(hints, "ESM modules")
	}

	if len(hints) == 0 {
		return ""
	}
	return "TypeScript configuration: " + strings.Join(hints, ", ") + "."
}

// ─── System Prompt Integration ──────────────────────────────────────────────

// FormatForSystemPrompt returns the rules formatted for injection into the LLM
// system prompt. Returns empty string if no rules are found.
func (pr *ProjectRules) FormatForSystemPrompt() string {
	if pr == nil || len(pr.Rules) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n## Project-Specific Rules\n")
	sb.WriteString("The following rules were loaded from this project's configuration files. ")
	sb.WriteString("You MUST follow these rules when writing or modifying code:\n\n")
	sb.WriteString(pr.Combined)
	return sb.String()
}

// HasRules returns true if any rules were discovered.
func (pr *ProjectRules) HasRules() bool {
	return pr != nil && len(pr.Rules) > 0
}
