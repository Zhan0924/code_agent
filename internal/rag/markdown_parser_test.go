package rag

import (
	"strings"
	"testing"
)

func TestParseMarkdown_BasicHeadings(t *testing.T) {
	md := `# Project Overview

This is the top-level intro paragraph.

## Architecture

The system uses microservices.

### Database Layer

We use PostgreSQL for persistence.

### Cache Layer

Redis is used for caching.

## Deployment

Deploy using Docker Compose.
`
	chunks := parseMarkdown(md)

	if len(chunks) == 0 {
		t.Fatal("expected chunks, got none")
	}

	// Should have: "Project Overview", "Architecture", "Database Layer", "Cache Layer", "Deployment"
	names := make([]string, len(chunks))
	for i, c := range chunks {
		names[i] = c.symbolName
	}
	t.Logf("Chunks: %v", names)

	// Verify heading hierarchy paths
	for _, c := range chunks {
		t.Logf("  [%s] %s (type=%s, lines=%d-%d, deps=%v)",
			c.symbolName, c.symbolType, c.symbolType, c.startLine, c.endLine, c.dependencies)
	}

	// Architecture section should exist
	found := false
	for _, c := range chunks {
		if c.symbolName == "Architecture" {
			found = true
			if c.symbolType != "section" {
				t.Errorf("expected type=section, got %s", c.symbolType)
			}
		}
	}
	if !found {
		t.Error("'Architecture' section not found")
	}

	// Database Layer should have heading path including parent
	for _, c := range chunks {
		if c.symbolName == "Database Layer" {
			if c.symbolType != "subsection" {
				t.Errorf("expected type=subsection, got %s", c.symbolType)
			}
			hasPath := false
			for _, d := range c.dependencies {
				if strings.Contains(d, "Architecture") && strings.Contains(d, "Database Layer") {
					hasPath = true
				}
			}
			if !hasPath {
				t.Errorf("Database Layer should have heading path containing 'Architecture > Database Layer', got deps=%v", c.dependencies)
			}
		}
	}
}

func TestParseMarkdown_CodeBlocksPreserved(t *testing.T) {
	md := "## Setup\n\nRun this:\n\n```bash\napt-get update\napt-get install -y docker\n```\n\nDone.\n"

	chunks := parseMarkdown(md)
	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}

	// The code block should be fully contained within the section
	for _, c := range chunks {
		if c.symbolName == "Setup" {
			if !strings.Contains(c.content, "apt-get update") {
				t.Error("code block content should be preserved in chunk")
			}
			if !strings.Contains(c.content, "apt-get install -y docker") {
				t.Error("full code block should be preserved")
			}
		}
	}
}

func TestParseMarkdown_HeadingInsideCodeBlock(t *testing.T) {
	md := "## Real Section\n\nSome text.\n\n```markdown\n# This is NOT a heading\n## Neither is this\n```\n\nMore text.\n"

	chunks := parseMarkdown(md)

	// Should only have 1 real section, not confused by headings inside code blocks
	realSections := 0
	for _, c := range chunks {
		if c.symbolName == "Real Section" {
			realSections++
		}
	}
	if realSections != 1 {
		t.Errorf("expected 1 'Real Section', got %d (headings inside code blocks should be ignored)", realSections)
	}

	// Should NOT have "This is NOT a heading" as a section
	for _, c := range chunks {
		if c.symbolName == "This is NOT a heading" {
			t.Error("heading inside code block was incorrectly parsed as a section")
		}
	}
}

func TestParseMarkdown_EmptyDocument(t *testing.T) {
	chunks := parseMarkdown("")
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty doc, got %d", len(chunks))
	}
}

func TestParseMarkdown_NoHeadings(t *testing.T) {
	md := "This is just a paragraph with no headings.\n\nAnother paragraph here.\n"

	chunks := parseMarkdown(md)
	// Should produce at least one chunk (preamble)
	if len(chunks) == 0 {
		t.Fatal("expected at least one preamble chunk for heading-less document")
	}

	if chunks[0].symbolType != "preamble" {
		t.Errorf("expected preamble type, got %s", chunks[0].symbolType)
	}
}

func TestParseMarkdown_DeepHierarchy(t *testing.T) {
	md := `# Root

## Chapter 1

### Section 1.1

#### Subsection 1.1.1

Deep content here with enough text to not be merged.
This paragraph contains details about the subsection topic.

### Section 1.2

Another section with sufficient content.
More paragraphs to ensure it's not merged.
`
	chunks := parseMarkdown(md)

	// Find the deep subsection and verify its path
	for _, c := range chunks {
		if c.symbolName == "Subsection 1.1.1" {
			for _, d := range c.dependencies {
				if strings.HasPrefix(d, "path:") {
					path := strings.TrimPrefix(d, "path:")
					// Should contain the full hierarchy
					if !strings.Contains(path, "Root") {
						t.Errorf("expected path to contain 'Root', got: %s", path)
					}
					if !strings.Contains(path, "Chapter 1") {
						t.Errorf("expected path to contain 'Chapter 1', got: %s", path)
					}
					if !strings.Contains(path, "Section 1.1") {
						t.Errorf("expected path to contain 'Section 1.1', got: %s", path)
					}
				}
			}
		}
	}
}

func TestParseMarkdown_CodeLanguageExtraction(t *testing.T) {
	md := "## API Example\n\nCall the API:\n\n```python\nimport requests\nresponse = requests.get('http://api.example.com')\nprint(response.json())\n```\n\nEnd.\n"

	chunks := parseMarkdown(md)

	for _, c := range chunks {
		if c.symbolName == "API Example" {
			hasLang := false
			for _, d := range c.dependencies {
				if d == "lang:python" {
					hasLang = true
				}
			}
			if !hasLang {
				t.Errorf("expected 'lang:python' in dependencies, got %v", c.dependencies)
			}
		}
	}
}

func TestParseShellScript_Functions(t *testing.T) {
	script := `#!/bin/bash
# Test script header

setup() {
    echo "setting up"
    mkdir -p /tmp/test
}

teardown() {
    echo "tearing down"
    rm -rf /tmp/test
}

run_tests() {
    setup
    echo "running tests"
    teardown
}
`
	chunks := parseShellScript(script)
	if len(chunks) == 0 {
		t.Fatal("expected shell function chunks")
	}

	funcNames := make(map[string]bool)
	for _, c := range chunks {
		funcNames[c.symbolName] = true
		t.Logf("  Shell chunk: %s (type=%s, lines=%d-%d)", c.symbolName, c.symbolType, c.startLine, c.endLine)
	}

	for _, expected := range []string{"setup", "teardown", "run_tests"} {
		if !funcNames[expected] {
			t.Errorf("expected function '%s' not found", expected)
		}
	}
}

func TestParseShellScript_NoFunctions(t *testing.T) {
	script := `#!/bin/bash
echo "hello"
ls -la
pwd
`
	chunks := parseShellScript(script)
	// Scripts without functions should return nil (fallback to sliding window)
	if chunks != nil {
		t.Errorf("expected nil for scripts without functions, got %d chunks", len(chunks))
	}
}

func TestParseShellScript_Header(t *testing.T) {
	script := `#!/bin/bash
# ============================================
# Deployment Script for Production
# Author: Team
# ============================================

deploy() {
    echo "deploying"
}
`
	chunks := parseShellScript(script)

	hasHeader := false
	for _, c := range chunks {
		if c.symbolType == "preamble" {
			hasHeader = true
			if !strings.Contains(c.content, "Deployment Script") {
				t.Error("header should contain script description")
			}
		}
	}
	if !hasHeader {
		t.Error("expected preamble chunk with script header comments")
	}
}

func TestParseATXHeading(t *testing.T) {
	tests := []struct {
		line          string
		expectedLevel int
		expectedTitle string
	}{
		{"# Title", 1, "Title"},
		{"## Section", 2, "Section"},
		{"### Sub Section", 3, "Sub Section"},
		{"#### Deep", 4, "Deep"},
		{"###### Max Depth", 6, "Max Depth"},
		{"####### Too Deep", 0, ""},       // Level 7 is invalid
		{"#notaheading", 0, ""},           // No space after #
		{"Normal text", 0, ""},            // Not a heading
		{"  ## Indented", 2, "Indented"},  // Indented heading
		{"## Trailing ##", 2, "Trailing"}, // Trailing markers
	}

	for _, tt := range tests {
		level, title := parseATXHeading(tt.line)
		if level != tt.expectedLevel {
			t.Errorf("parseATXHeading(%q): level=%d, want %d", tt.line, level, tt.expectedLevel)
		}
		if title != tt.expectedTitle {
			t.Errorf("parseATXHeading(%q): title=%q, want %q", tt.line, title, tt.expectedTitle)
		}
	}
}

func TestUpdateHeadingStack(t *testing.T) {
	var stack []headingEntry

	// Add H1
	stack = updateHeadingStack(stack, 1, "Root")
	if len(stack) != 1 || stack[0].title != "Root" {
		t.Errorf("after H1: %v", stack)
	}

	// Add H2 (child of H1)
	stack = updateHeadingStack(stack, 2, "Chapter")
	if len(stack) != 2 {
		t.Errorf("after H2: expected 2 entries, got %d", len(stack))
	}

	// Add H3 (child of H2)
	stack = updateHeadingStack(stack, 3, "Section")
	if len(stack) != 3 {
		t.Errorf("after H3: expected 3 entries, got %d", len(stack))
	}

	// Add another H2 (should pop H3 and replace H2)
	stack = updateHeadingStack(stack, 2, "Chapter 2")
	if len(stack) != 2 {
		t.Errorf("after new H2: expected 2 entries, got %d", len(stack))
	}
	if stack[1].title != "Chapter 2" {
		t.Errorf("top of stack should be 'Chapter 2', got %q", stack[1].title)
	}
}

func TestBuildHeadingPath(t *testing.T) {
	stack := []headingEntry{
		{level: 1, title: "Architecture"},
		{level: 2, title: "Deployment"},
		{level: 3, title: "HA Design"},
	}

	path := buildHeadingPath(stack, "HA Design", 3)
	expected := "Architecture > Deployment > HA Design"
	if path != expected {
		t.Errorf("expected %q, got %q", expected, path)
	}
}

func TestIsValidShellIdentifier(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"setup", true},
		{"my_func", true},
		{"run-tests", true},
		{"_private", true},
		{"123bad", false},
		{"", false},
		{"$var", false},
	}

	for _, tt := range tests {
		if got := isValidShellIdentifier(tt.input); got != tt.valid {
			t.Errorf("isValidShellIdentifier(%q) = %v, want %v", tt.input, got, tt.valid)
		}
	}
}
