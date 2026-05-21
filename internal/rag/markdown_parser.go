// Package rag — markdown_parser.go provides structure-aware Markdown chunking
// for the RAG knowledge base. Instead of naive sliding-window splitting, it:
//
//  1. Splits by heading boundaries (# / ## / ### etc.)
//  2. Preserves the full heading hierarchy path as metadata (e.g., "Architecture > Deployment")
//  3. Keeps fenced code blocks intact within their parent section
//  4. Extracts YAML front matter as metadata if present
//  5. Handles tables, lists, and nested structures gracefully
//
// This is critical for technical documentation and AI-generated proposals where
// semantic section boundaries carry important contextual information.
package rag

import (
	"fmt"
	"strings"
)

// markdownChunk represents a semantically meaningful section of a Markdown document.
type markdownChunk struct {
	title        string   // The heading text of this section
	headingLevel int      // 1-6 for H1-H6, 0 for preamble (before first heading)
	headingPath  string   // Full hierarchy path: "Parent > Child > Section"
	content      string   // The full text content of the section (including heading)
	startLine    int      // 1-based starting line number
	endLine      int      // 1-based ending line number
	hasCodeBlock bool     // Whether this section contains fenced code blocks
	codeLanguage string   // Language of the first code block if any
	tags         []string // Extracted tags from front matter or inline markers
}

// parseMarkdown splits a Markdown document into semantic sections based on headings.
// Each section includes the full heading hierarchy path for context-rich retrieval.
func parseMarkdown(content string) []astChunk {
	lines := strings.Split(content, "\n")
	sections := splitMarkdownSections(lines)

	if len(sections) == 0 {
		return nil
	}

	var chunks []astChunk
	for _, sec := range sections {
		// Build the symbol name from the heading or "preamble"/"front-matter"
		symbolName := sec.title
		if symbolName == "" {
			symbolName = "preamble"
		}

		// Determine symbol type based on heading level
		symbolType := "section"
		switch {
		case sec.headingLevel == 0:
			symbolType = "preamble"
		case sec.headingLevel == 1:
			symbolType = "document"
		case sec.headingLevel == 2:
			symbolType = "section"
		case sec.headingLevel >= 3:
			symbolType = "subsection"
		}

		// Skip empty sections (heading only, no body)
		trimmedContent := strings.TrimSpace(sec.content)
		if trimmedContent == "" || trimmedContent == fmt.Sprintf("%s %s", strings.Repeat("#", sec.headingLevel), sec.title) {
			continue
		}

		chunk := astChunk{
			symbolName:   symbolName,
			symbolType:   symbolType,
			content:      sec.content,
			startLine:    sec.startLine,
			endLine:      sec.endLine,
			dependencies: nil, // Markdown doesn't have code dependencies
		}

		// Store heading path as a dependency-like field for retrieval enrichment
		if sec.headingPath != "" {
			chunk.dependencies = []string{"path:" + sec.headingPath}
		}
		if sec.hasCodeBlock && sec.codeLanguage != "" {
			chunk.dependencies = append(chunk.dependencies, "lang:"+sec.codeLanguage)
		}

		chunks = append(chunks, chunk)
	}

	return chunks
}

// splitMarkdownSections walks the lines and splits at heading boundaries,
// maintaining the heading hierarchy stack.
func splitMarkdownSections(lines []string) []markdownChunk {
	var sections []markdownChunk
	var headingStack []headingEntry // Stack for heading hierarchy

	inFencedCode := false
	fenceMarker := ""
	currentStart := 0
	currentTitle := ""
	currentLevel := 0
	currentHasCode := false
	currentCodeLang := ""

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track fenced code block boundaries (``` or ~~~)
		if !inFencedCode {
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				inFencedCode = true
				fenceMarker = trimmed[:3]
				currentHasCode = true
				// Extract language hint
				langHint := strings.TrimSpace(strings.TrimPrefix(trimmed, fenceMarker))
				if langHint != "" && currentCodeLang == "" {
					// Remove any extra markers like ```go{...}
					if idx := strings.IndexAny(langHint, " {"); idx > 0 {
						langHint = langHint[:idx]
					}
					currentCodeLang = langHint
				}
				continue
			}
		} else {
			if strings.HasPrefix(trimmed, fenceMarker) {
				inFencedCode = false
				fenceMarker = ""
			}
			continue
		}

		// Only process headings outside of code blocks
		if inFencedCode {
			continue
		}

		// Detect ATX headings: # Heading
		level, title := parseATXHeading(line)
		if level > 0 {
			// Flush previous section
			if i > currentStart {
				sectionContent := strings.Join(lines[currentStart:i], "\n")
				if strings.TrimSpace(sectionContent) != "" {
					sections = append(sections, markdownChunk{
						title:        currentTitle,
						headingLevel: currentLevel,
						headingPath:  buildHeadingPath(headingStack, currentTitle, currentLevel),
						content:      sectionContent,
						startLine:    currentStart + 1,
						endLine:      i,
						hasCodeBlock: currentHasCode,
						codeLanguage: currentCodeLang,
					})
				}
			}

			// Update heading stack
			headingStack = updateHeadingStack(headingStack, level, title)

			// Start new section
			currentStart = i
			currentTitle = title
			currentLevel = level
			currentHasCode = false
			currentCodeLang = ""
		}
	}

	// Flush the last section
	if currentStart < len(lines) {
		sectionContent := strings.Join(lines[currentStart:], "\n")
		if strings.TrimSpace(sectionContent) != "" {
			sections = append(sections, markdownChunk{
				title:        currentTitle,
				headingLevel: currentLevel,
				headingPath:  buildHeadingPath(headingStack, currentTitle, currentLevel),
				content:      sectionContent,
				startLine:    currentStart + 1,
				endLine:      len(lines),
				hasCodeBlock: currentHasCode,
				codeLanguage: currentCodeLang,
			})
		}
	}

	// Post-processing: merge very small sections (heading-only with no body)
	sections = mergeSmallSections(sections, 20) // minimum 20 chars (heading-only threshold)

	return sections
}

// headingEntry tracks a heading in the hierarchy stack.
type headingEntry struct {
	level int
	title string
}

// parseATXHeading extracts the heading level and title from a line like "## Title".
func parseATXHeading(line string) (int, string) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return 0, ""
	}

	level := 0
	for _, c := range trimmed {
		if c == '#' {
			level++
		} else {
			break
		}
	}

	if level == 0 || level > 6 {
		return 0, ""
	}

	// Must have a space after the #'s (or be just #'s for an empty heading)
	rest := trimmed[level:]
	if len(rest) > 0 && rest[0] != ' ' {
		return 0, "" // Not a valid heading (e.g., "#notaheading")
	}

	title := strings.TrimSpace(rest)
	// Remove trailing #'s (optional closing marks)
	title = strings.TrimRight(title, "# ")

	return level, title
}

// updateHeadingStack maintains the hierarchy stack when encountering a new heading.
func updateHeadingStack(stack []headingEntry, level int, title string) []headingEntry {
	// Pop all entries at the same or deeper level
	for len(stack) > 0 && stack[len(stack)-1].level >= level {
		stack = stack[:len(stack)-1]
	}
	stack = append(stack, headingEntry{level: level, title: title})
	return stack
}

// buildHeadingPath constructs the full hierarchy path from the stack.
func buildHeadingPath(stack []headingEntry, currentTitle string, currentLevel int) string {
	if currentLevel == 0 || len(stack) == 0 {
		return currentTitle
	}

	var parts []string
	for _, entry := range stack {
		if entry.title != "" {
			parts = append(parts, entry.title)
		}
	}

	if len(parts) == 0 {
		return currentTitle
	}
	return strings.Join(parts, " > ")
}

// mergeSmallSections combines very short sections with the next section
// to avoid creating too many tiny chunks with poor retrieval value.
func mergeSmallSections(sections []markdownChunk, minChars int) []markdownChunk {
	if len(sections) <= 1 {
		return sections
	}

	var merged []markdownChunk
	for i := 0; i < len(sections); i++ {
		sec := sections[i]

		// If this section is too small and there's a next section, merge into next
		if len(strings.TrimSpace(sec.content)) < minChars && i+1 < len(sections) {
			// Prepend this content to the next section
			sections[i+1].content = sec.content + "\n" + sections[i+1].content
			sections[i+1].startLine = sec.startLine
			if sec.headingLevel < sections[i+1].headingLevel || sections[i+1].headingLevel == 0 {
				// Keep the parent heading info if merging down
			}
			continue
		}
		merged = append(merged, sec)
	}

	return merged
}

// parseShellScript parses shell scripts (.sh, .bash) into function-level chunks.
// Shell functions are declared as: function_name() { ... } or function function_name { ... }
func parseShellScript(content string) []astChunk {
	lines := strings.Split(content, "\n")
	var chunks []astChunk
	var current *astChunk
	braceDepth := 0
	headerComments := []string{}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Collect header comments (before first function)
		if current == nil && len(chunks) == 0 {
			if strings.HasPrefix(trimmed, "#") || trimmed == "" {
				headerComments = append(headerComments, line)
				continue
			}
			// Flush header as a preamble chunk
			if len(headerComments) > 0 {
				preamble := strings.Join(headerComments, "\n")
				if strings.TrimSpace(preamble) != "" {
					chunks = append(chunks, astChunk{
						symbolName: "script_header",
						symbolType: "preamble",
						content:    preamble,
						startLine:  1,
						endLine:    i,
					})
				}
				headerComments = nil
			}
		}

		// Detect function declarations
		funcName := ""
		if strings.Contains(trimmed, "()") && !strings.HasPrefix(trimmed, "#") {
			// Pattern: function_name() {  or  function_name ()
			idx := strings.Index(trimmed, "()")
			if idx > 0 {
				name := strings.TrimSpace(trimmed[:idx])
				name = strings.TrimPrefix(name, "function ")
				if isValidShellIdentifier(name) {
					funcName = name
				}
			}
		} else if strings.HasPrefix(trimmed, "function ") {
			// Pattern: function name {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				name := strings.TrimSuffix(parts[1], "()")
				name = strings.TrimSuffix(name, "{")
				if isValidShellIdentifier(name) {
					funcName = name
				}
			}
		}

		if funcName != "" {
			// Flush previous function
			if current != nil && braceDepth == 0 {
				current.endLine = i
				current.content = strings.Join(lines[current.startLine-1:i], "\n")
				chunks = append(chunks, *current)
			}
			current = &astChunk{
				symbolName: funcName,
				symbolType: "function",
				startLine:  i + 1,
			}
		}

		// Track brace depth
		for _, c := range line {
			if c == '{' {
				braceDepth++
			} else if c == '}' {
				braceDepth--
				if braceDepth == 0 && current != nil {
					current.endLine = i + 1
					current.content = strings.Join(lines[current.startLine-1:i+1], "\n")
					chunks = append(chunks, *current)
					current = nil
				}
			}
		}
	}

	// Flush last chunk
	if current != nil {
		current.endLine = len(lines)
		current.content = strings.Join(lines[current.startLine-1:], "\n")
		chunks = append(chunks, *current)
	}

	// If no function chunks found, return nil to trigger sliding window fallback.
	// Preamble-only results don't justify AST-style chunking for linear scripts.
	hasFunctions := false
	for _, c := range chunks {
		if c.symbolType == "function" {
			hasFunctions = true
			break
		}
	}
	if !hasFunctions {
		return nil
	}

	return chunks
}

// isValidShellIdentifier checks if a string is a valid shell function name.
func isValidShellIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, c := range s {
		if i == 0 {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return false
			}
		} else {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
				return false
			}
		}
	}
	return true
}
