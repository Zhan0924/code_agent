//go:build !tree_sitter

package treesitter

import (
	"fmt"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

// fallbackParser implements Parser using regex heuristics when tree-sitter CGO is unavailable.
type fallbackParser struct {
	logger *zap.Logger
}

// NewCGOParser returns a fallback parser when tree_sitter build tag is not set.
func NewCGOParser(logger *zap.Logger) Parser {
	logger.Warn("tree-sitter CGO not available, using regex fallback")
	return &fallbackParser{logger: logger.With(zap.String("component", "treesitter-fallback"))}
}

func (p *fallbackParser) SupportedLanguages() []string {
	return []string{"go", "python", "typescript", "javascript", "rust", "java", "c", "cpp"}
}

func (p *fallbackParser) ExtractSymbols(language, content string) ([]Symbol, error) {
	lang := strings.ToLower(language)
	patterns := symbolPatterns[lang]
	if len(patterns) == 0 {
		return nil, fmt.Errorf("unsupported language for fallback: %s", language)
	}

	lines := strings.Split(content, "\n")
	var symbols []Symbol

	for i, line := range lines {
		for _, sp := range patterns {
			if matches := sp.re.FindStringSubmatch(line); len(matches) > 1 {
				name := matches[1]
				vis := "public"
				if sp.visCheck != nil && !sp.visCheck(name) {
					vis = "private"
				}
				symbols = append(symbols, Symbol{
					Name:       name,
					Kind:       sp.kind,
					StartLine:  i + 1,
					EndLine:    i + 1,
					Visibility: vis,
				})
			}
		}
	}

	return symbols, nil
}

func (p *fallbackParser) ChunkByAST(language, content string) ([]Chunk, error) {
	symbols, err := p.ExtractSymbols(language, content)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(content, "\n")
	var chunks []Chunk

	for i, sym := range symbols {
		endLine := sym.StartLine
		if i+1 < len(symbols) {
			endLine = symbols[i+1].StartLine - 1
		} else {
			endLine = len(lines)
		}

		startIdx := sym.StartLine - 1
		endIdx := endLine
		if startIdx < 0 {
			startIdx = 0
		}
		if endIdx > len(lines) {
			endIdx = len(lines)
		}

		chunks = append(chunks, Chunk{
			SymbolName: sym.Name,
			SymbolType: sym.Kind,
			Content:    strings.Join(lines[startIdx:endIdx], "\n"),
			StartLine:  sym.StartLine,
			EndLine:    endLine,
		})
	}

	if len(chunks) == 0 && len(content) > 0 {
		chunks = append(chunks, Chunk{
			SymbolName: "<file>",
			SymbolType: "file",
			Content:    content,
			StartLine:  1,
			EndLine:    len(lines),
		})
	}

	return chunks, nil
}

type symbolPattern struct {
	re       *regexp.Regexp
	kind     string
	visCheck func(string) bool
}

var symbolPatterns = map[string][]symbolPattern{
	"go": {
		{re: regexp.MustCompile(`^func\s+(\w+)\s*\(`), kind: "function", visCheck: isGoExported},
		{re: regexp.MustCompile(`^func\s+\([^)]+\)\s+(\w+)\s*\(`), kind: "method", visCheck: isGoExported},
		{re: regexp.MustCompile(`^type\s+(\w+)\s+struct\b`), kind: "struct", visCheck: isGoExported},
		{re: regexp.MustCompile(`^type\s+(\w+)\s+interface\b`), kind: "interface", visCheck: isGoExported},
	},
	"python": {
		{re: regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)\s*\(`), kind: "function", visCheck: isPythonPublic},
		{re: regexp.MustCompile(`^class\s+(\w+)`), kind: "class"},
	},
	"typescript": {
		{re: regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+(\w+)`), kind: "function"},
		{re: regexp.MustCompile(`^(?:export\s+)?class\s+(\w+)`), kind: "class"},
		{re: regexp.MustCompile(`^(?:export\s+)?interface\s+(\w+)`), kind: "interface"},
	},
	"javascript": {
		{re: regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+(\w+)`), kind: "function"},
		{re: regexp.MustCompile(`^(?:export\s+)?class\s+(\w+)`), kind: "class"},
	},
	"rust": {
		{re: regexp.MustCompile(`^(?:pub\s+)?fn\s+(\w+)`), kind: "function"},
		{re: regexp.MustCompile(`^(?:pub\s+)?struct\s+(\w+)`), kind: "struct"},
		{re: regexp.MustCompile(`^(?:pub\s+)?trait\s+(\w+)`), kind: "interface"},
		{re: regexp.MustCompile(`^(?:pub\s+)?enum\s+(\w+)`), kind: "type"},
		{re: regexp.MustCompile(`^impl(?:<[^>]+>)?\s+(\w+)`), kind: "type"},
	},
	"java": {
		{re: regexp.MustCompile(`(?:public|private|protected)\s+(?:static\s+)?(?:[\w<>\[\]]+\s+)?(\w+)\s*\(`), kind: "method"},
		{re: regexp.MustCompile(`(?:public\s+)?class\s+(\w+)`), kind: "class"},
		{re: regexp.MustCompile(`(?:public\s+)?interface\s+(\w+)`), kind: "interface"},
	},
	"c": {
		{re: regexp.MustCompile(`^(?:\w+\s+)+(\w+)\s*\([^;]*$`), kind: "function"},
		{re: regexp.MustCompile(`^(?:typedef\s+)?struct\s+(\w+)`), kind: "struct"},
	},
	"cpp": {
		{re: regexp.MustCompile(`^(?:\w+\s+)+(\w+)\s*\([^;]*$`), kind: "function"},
		{re: regexp.MustCompile(`^class\s+(\w+)`), kind: "class"},
		{re: regexp.MustCompile(`^(?:typedef\s+)?struct\s+(\w+)`), kind: "struct"},
	},
}

func isGoExported(name string) bool {
	return len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
}

func isPythonPublic(name string) bool {
	return !strings.HasPrefix(name, "_")
}
