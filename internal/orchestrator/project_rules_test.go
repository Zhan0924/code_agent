package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestRuleLoader_LoadCoderules(t *testing.T) {
	dir := t.TempDir()
	content := "Always use Go error wrapping with fmt.Errorf and %w"
	if err := os.WriteFile(filepath.Join(dir, ".coderules"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewRuleLoader(zap.NewNop())
	rules := loader.Load(dir)

	if !rules.HasRules() {
		t.Fatal("expected rules to be found")
	}
	if len(rules.Rules) != 1 {
		t.Errorf("expected 1 rule file, got %d", len(rules.Rules))
	}
	if rules.Rules[0].Source != ".coderules" {
		t.Errorf("expected source .coderules, got %s", rules.Rules[0].Source)
	}
	if !strings.Contains(rules.Combined, "error wrapping") {
		t.Error("combined should contain rule content")
	}
}

func TestRuleLoader_LoadMultipleFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".coderules"), []byte("Rule 1"), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Rule 2"), 0o644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Rule 3"), 0o644)

	loader := NewRuleLoader(zap.NewNop())
	rules := loader.Load(dir)

	if len(rules.Rules) != 3 {
		t.Errorf("expected 3 rule files, got %d", len(rules.Rules))
	}
}

func TestRuleLoader_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	loader := NewRuleLoader(zap.NewNop())
	rules := loader.Load(dir)

	if rules.HasRules() {
		t.Error("expected no rules in empty directory")
	}
	if rules.FormatForSystemPrompt() != "" {
		t.Error("expected empty system prompt for no rules")
	}
}

func TestRuleLoader_Caching(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".coderules"), []byte("cached rule"), 0o644)

	loader := NewRuleLoader(zap.NewNop())
	r1 := loader.Load(dir)
	r2 := loader.Load(dir)

	// Should be the same pointer (cached)
	if r1 != r2 {
		t.Error("expected cached result")
	}

	// Invalidate
	loader.Invalidate(dir)
	r3 := loader.Load(dir)
	if r3 == r1 {
		t.Error("expected fresh result after invalidation")
	}
}

func TestRuleLoader_TruncatesLargeFile(t *testing.T) {
	dir := t.TempDir()
	// Create a 10KB file
	large := strings.Repeat("x", 10000)
	os.WriteFile(filepath.Join(dir, ".coderules"), []byte(large), 0o644)

	loader := NewRuleLoader(zap.NewNop())
	rules := loader.Load(dir)

	if len(rules.Rules[0].Content) > 4200 {
		t.Errorf("expected truncation, got %d bytes", len(rules.Rules[0].Content))
	}
	if !strings.Contains(rules.Rules[0].Content, "truncated") {
		t.Error("expected truncation marker")
	}
}

func TestRuleLoader_SkipsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".coderules"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Real rule"), 0o644)

	loader := NewRuleLoader(zap.NewNop())
	rules := loader.Load(dir)

	if len(rules.Rules) != 1 {
		t.Errorf("expected 1 rule (skipping empty), got %d", len(rules.Rules))
	}
}

func TestProjectRules_FormatForSystemPrompt(t *testing.T) {
	rules := &ProjectRules{
		Rules: []RuleFile{
			{Source: ".coderules", Content: "Use Go idioms"},
		},
		Combined: "### Rules from .coderules:\nUse Go idioms\n\n",
	}

	prompt := rules.FormatForSystemPrompt()
	if !strings.Contains(prompt, "Project-Specific Rules") {
		t.Error("expected header in system prompt")
	}
	if !strings.Contains(prompt, "Use Go idioms") {
		t.Error("expected rule content in prompt")
	}
	if !strings.Contains(prompt, "MUST follow") {
		t.Error("expected enforcement language")
	}
}

func TestProjectRules_NilSafe(t *testing.T) {
	var rules *ProjectRules
	if rules.HasRules() {
		t.Error("nil rules should not have rules")
	}
	if rules.FormatForSystemPrompt() != "" {
		t.Error("nil rules should return empty prompt")
	}
}

// ─── Language-specific summarizer tests ─────────────────────────────────────

func TestExtractGolangciSummary(t *testing.T) {
	content := `linters:
  enable:
    - errcheck
    - govet
    - staticcheck
    - gosec
`
	summary := extractGolangciSummary(content)
	if !strings.Contains(summary, "errcheck") {
		t.Error("should detect errcheck")
	}
	if !strings.Contains(summary, "staticcheck") {
		t.Error("should detect staticcheck")
	}
	if !strings.Contains(summary, "gosec") {
		t.Error("should detect gosec")
	}
}

func TestExtractGolangciSummary_Empty(t *testing.T) {
	if extractGolangciSummary("") != "" {
		t.Error("empty content should return empty summary")
	}
}

func TestExtractPythonToolSummary(t *testing.T) {
	content := `[tool.ruff]
line-length = 120

[tool.mypy]
strict = true
`
	summary := extractPythonToolSummary(content)
	if !strings.Contains(summary, "ruff") {
		t.Error("should detect ruff")
	}
	if !strings.Contains(summary, "mypy") {
		t.Error("should detect mypy")
	}
}

func TestExtractTSConfigSummary(t *testing.T) {
	content := `{
  "compilerOptions": {
    "strict": true,
    "jsx": "react-jsx",
    "module": "esnext"
  }
}`
	summary := extractTSConfigSummary(content)
	if !strings.Contains(summary, "strict mode") {
		t.Error("should detect strict mode")
	}
	if !strings.Contains(summary, "JSX") {
		t.Error("should detect JSX")
	}
	if !strings.Contains(summary, "ESM") {
		t.Error("should detect ESM modules")
	}
}

func TestRuleLoader_GolangciIntegration(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".golangci.yml"), []byte("linters:\n  enable:\n    - errcheck\n"), 0o644)

	loader := NewRuleLoader(zap.NewNop())
	rules := loader.Load(dir)

	if !rules.HasRules() {
		t.Fatal("expected rules from .golangci.yml")
	}
	if !strings.Contains(rules.Combined, "errcheck") {
		t.Error("expected errcheck in combined rules")
	}
}
