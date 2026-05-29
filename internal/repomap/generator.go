// Package repomap generates a high-level repository structure map
// (similar to Aider's repo-map) that gives the LLM an overview of
// the codebase architecture without sending entire file contents.
package repomap

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ─── Generator ──────────────────────────────────────────────────────────────

// Generator builds a compact repo-map from a directory tree.
type Generator struct {
	logger   *zap.Logger
	tsParser TreeSitterParser // optional tree-sitter parser for enhanced symbol extraction

	mu       sync.RWMutex
	cache    map[string]*CachedMap // rootDir → cached map
	cacheTTL time.Duration
}

// TreeSitterParser is the interface for tree-sitter based symbol extraction.
type TreeSitterParser interface {
	ExtractSymbols(language, content string) ([]TreeSitterSymbol, error)
	SupportedLanguages() []string
}

// TreeSitterSymbol represents a symbol extracted by tree-sitter.
type TreeSitterSymbol struct {
	Name       string
	Kind       string
	StartLine  int
	Visibility string
}

// CachedMap holds a cached repo map with expiry.
type CachedMap struct {
	Content   string
	Entries   []FileEntry
	CreatedAt time.Time
}

// FileEntry represents a single file in the repo map with its public symbols.
type FileEntry struct {
	Path    string   `json:"path"`
	Lang    string   `json:"lang"`
	Symbols []Symbol `json:"symbols,omitempty"`
	Lines   int      `json:"lines"`
}

// Symbol represents a public definition (func, type, const, class, def).
type Symbol struct {
	Kind string `json:"kind"` // "func", "type", "class", "def", "const", "interface"
	Name string `json:"name"`
	Line int    `json:"line"`
}

// NewGenerator creates a repo map generator.
func NewGenerator(logger *zap.Logger) *Generator {
	return &Generator{
		logger:   logger,
		cache:    make(map[string]*CachedMap),
		cacheTTL: 5 * time.Minute,
	}
}

// SetTreeSitterParser sets the tree-sitter parser for enhanced symbol extraction.
func (g *Generator) SetTreeSitterParser(p TreeSitterParser) {
	g.tsParser = p
}

// Generate produces a compact text repo-map for the given directory.
// It extracts public symbols from supported languages (Go, Python, TS/JS, Rust).
func (g *Generator) Generate(rootDir string) (string, error) {
	// Check cache
	g.mu.RLock()
	if cached, ok := g.cache[rootDir]; ok && time.Since(cached.CreatedAt) < g.cacheTTL {
		g.mu.RUnlock()
		return cached.Content, nil
	}
	g.mu.RUnlock()

	entries, err := g.scanDir(rootDir)
	if err != nil {
		return "", err
	}

	content := g.formatMap(rootDir, entries)

	// Update cache
	g.mu.Lock()
	g.cache[rootDir] = &CachedMap{
		Content:   content,
		Entries:   entries,
		CreatedAt: time.Now(),
	}
	g.mu.Unlock()

	return content, nil
}

// InvalidateCache clears the cache for a specific root directory.
func (g *Generator) InvalidateCache(rootDir string) {
	g.mu.Lock()
	delete(g.cache, rootDir)
	g.mu.Unlock()
}

// InvalidateFile removes cache when a specific file changes.
func (g *Generator) InvalidateFile(rootDir, _ string) {
	g.InvalidateCache(rootDir)
}

// GetEntries returns the structured file entries for a directory.
func (g *Generator) GetEntries(rootDir string) ([]FileEntry, error) {
	g.mu.RLock()
	if cached, ok := g.cache[rootDir]; ok && time.Since(cached.CreatedAt) < g.cacheTTL {
		g.mu.RUnlock()
		return cached.Entries, nil
	}
	g.mu.RUnlock()

	// Force regeneration to populate cache
	if _, err := g.Generate(rootDir); err != nil {
		return nil, err
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	if cached, ok := g.cache[rootDir]; ok {
		return cached.Entries, nil
	}
	return nil, nil
}

// ─── Directory Scanning ─────────────────────────────────────────────────────

// skipDirs are directories to exclude from scanning.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	".idea": true, ".vscode": true, "__pycache__": true,
	"dist": true, "build": true, "target": true,
	".next": true, ".cache": true, "bin": true,
}

// supportedExts maps file extensions to language identifiers.
var supportedExts = map[string]string{
	".go":   "go",
	".py":   "python",
	".ts":   "typescript",
	".tsx":  "typescript",
	".js":   "javascript",
	".jsx":  "javascript",
	".rs":   "rust",
	".java": "java",
}

func (g *Generator) scanDir(rootDir string) ([]FileEntry, error) {
	var entries []FileEntry

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}

		// Skip hidden dirs and known non-code dirs
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") || skipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip large files, binary files, test files
		if info.Size() > 500*1024 { // > 500KB
			return nil
		}

		ext := filepath.Ext(path)
		lang, supported := supportedExts[ext]
		if !supported {
			return nil
		}

		rel, _ := filepath.Rel(rootDir, path)
		entry := FileEntry{
			Path: rel,
			Lang: lang,
		}

		// Extract symbols (try tree-sitter first if available)
		symbols, lines, err := g.extractSymbolsWithParser(path, lang)
		if err != nil {
			g.logger.Debug("failed to extract symbols", zap.String("file", rel), zap.Error(err))
		} else {
			entry.Symbols = symbols
			entry.Lines = lines
		}

		entries = append(entries, entry)
		return nil
	})

	// Sort by path for deterministic output
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	return entries, err
}

// ─── Symbol Extraction (regex-based, fast) ──────────────────────────────────

// Language-specific regex patterns for public symbol extraction.
var (
	goFuncRe      = regexp.MustCompile(`^func\s+(?:\(.*?\)\s+)?([A-Z]\w*)\s*\(`)
	goTypeRe      = regexp.MustCompile(`^type\s+([A-Z]\w*)\s+`)
	goInterfaceRe = regexp.MustCompile(`^type\s+([A-Z]\w*)\s+interface\s*\{`)
	goConstRe     = regexp.MustCompile(`^(?:const|var)\s+([A-Z]\w*)`)

	pyClassRe = regexp.MustCompile(`^class\s+(\w+)`)
	pyDefRe   = regexp.MustCompile(`^def\s+(\w+)`)
	pyAsyncRe = regexp.MustCompile(`^async\s+def\s+(\w+)`)

	tsClassRe     = regexp.MustCompile(`^(?:export\s+)?class\s+(\w+)`)
	tsFuncRe      = regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+(\w+)`)
	tsInterfaceRe = regexp.MustCompile(`^(?:export\s+)?interface\s+(\w+)`)
	tsConstRe     = regexp.MustCompile(`^(?:export\s+)?const\s+(\w+)`)
	tsTypeRe      = regexp.MustCompile(`^(?:export\s+)?type\s+(\w+)`)

	rsFnRe     = regexp.MustCompile(`^pub\s+(?:async\s+)?fn\s+(\w+)`)
	rsStructRe = regexp.MustCompile(`^pub\s+struct\s+(\w+)`)
	rsEnumRe   = regexp.MustCompile(`^pub\s+enum\s+(\w+)`)
	rsTraitRe  = regexp.MustCompile(`^pub\s+trait\s+(\w+)`)

	javaClassRe  = regexp.MustCompile(`^(?:public|protected)\s+(?:abstract\s+)?(?:class|interface|enum)\s+(\w+)`)
	javaMethodRe = regexp.MustCompile(`^\s+(?:public|protected)\s+.*?\s+(\w+)\s*\(`)
)

// extractSymbolsWithParser tries tree-sitter first, then falls back to regex extraction.
func (g *Generator) extractSymbolsWithParser(filePath, lang string) ([]Symbol, int, error) {
	if g.tsParser != nil {
		content, err := os.ReadFile(filePath)
		if err == nil {
			tsSymbols, err := g.tsParser.ExtractSymbols(lang, string(content))
			if err == nil && len(tsSymbols) > 0 {
				symbols := make([]Symbol, 0, len(tsSymbols))
				for _, ts := range tsSymbols {
					// Only include public symbols (matching repomap's purpose)
					if ts.Visibility == "public" || ts.Visibility == "" {
						symbols = append(symbols, Symbol{
							Kind: ts.Kind,
							Name: ts.Name,
							Line: ts.StartLine,
						})
					}
				}
				// Count lines
				lines := len(strings.Split(string(content), "\n"))
				return symbols, lines, nil
			}
		}
	}
	// Fall back to regex extraction
	return extractSymbols(filePath, lang)
}

func extractSymbols(filePath, lang string) ([]Symbol, int, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var symbols []Symbol
	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		switch lang {
		case "go":
			if m := goInterfaceRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "interface", Name: m[1], Line: lineNum})
			} else if m := goTypeRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "type", Name: m[1], Line: lineNum})
			} else if m := goFuncRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "func", Name: m[1], Line: lineNum})
			} else if m := goConstRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "const", Name: m[1], Line: lineNum})
			}

		case "python":
			if m := pyClassRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "class", Name: m[1], Line: lineNum})
			} else if m := pyAsyncRe.FindStringSubmatch(trimmed); m != nil {
				if !strings.HasPrefix(m[1], "_") {
					symbols = append(symbols, Symbol{Kind: "def", Name: m[1], Line: lineNum})
				}
			} else if m := pyDefRe.FindStringSubmatch(trimmed); m != nil {
				if !strings.HasPrefix(m[1], "_") {
					symbols = append(symbols, Symbol{Kind: "def", Name: m[1], Line: lineNum})
				}
			}

		case "typescript", "javascript":
			if m := tsClassRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "class", Name: m[1], Line: lineNum})
			} else if m := tsInterfaceRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "interface", Name: m[1], Line: lineNum})
			} else if m := tsTypeRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "type", Name: m[1], Line: lineNum})
			} else if m := tsFuncRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "func", Name: m[1], Line: lineNum})
			} else if m := tsConstRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "const", Name: m[1], Line: lineNum})
			}

		case "rust":
			if m := rsTraitRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "trait", Name: m[1], Line: lineNum})
			} else if m := rsStructRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "struct", Name: m[1], Line: lineNum})
			} else if m := rsEnumRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "enum", Name: m[1], Line: lineNum})
			} else if m := rsFnRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "func", Name: m[1], Line: lineNum})
			}

		case "java":
			if m := javaClassRe.FindStringSubmatch(trimmed); m != nil {
				symbols = append(symbols, Symbol{Kind: "class", Name: m[1], Line: lineNum})
			} else if m := javaMethodRe.FindStringSubmatch(line); m != nil {
				symbols = append(symbols, Symbol{Kind: "func", Name: m[1], Line: lineNum})
			}
		}
	}

	return symbols, lineNum, scanner.Err()
}

// ─── Formatting ─────────────────────────────────────────────────────────────

// formatMap renders the repo map as a compact text suitable for LLM context.
func (g *Generator) formatMap(rootDir string, entries []FileEntry) string {
	var sb strings.Builder
	baseName := filepath.Base(rootDir)
	sb.WriteString(fmt.Sprintf("# Repository Map: %s\n", baseName))
	sb.WriteString(fmt.Sprintf("# Files: %d | Generated: %s\n\n", len(entries), time.Now().Format("2006-01-02 15:04")))

	// Group by directory
	dirGroups := make(map[string][]FileEntry)
	for _, e := range entries {
		dir := filepath.Dir(e.Path)
		dirGroups[dir] = append(dirGroups[dir], e)
	}

	// Sort directories
	dirs := make([]string, 0, len(dirGroups))
	for d := range dirGroups {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		files := dirGroups[dir]
		sb.WriteString(fmt.Sprintf("## %s/\n", dir))

		for _, f := range files {
			name := filepath.Base(f.Path)
			sb.WriteString(fmt.Sprintf("  %s (%d lines)\n", name, f.Lines))

			for _, sym := range f.Symbols {
				sb.WriteString(fmt.Sprintf("    %s %s (L%d)\n", sym.Kind, sym.Name, sym.Line))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// FormatCompact returns a very compact map (just file paths + top symbol counts)
// suitable for tight token budgets.
func FormatCompact(entries []FileEntry) string {
	var sb strings.Builder
	for _, e := range entries {
		symCount := len(e.Symbols)
		if symCount > 0 {
			sb.WriteString(fmt.Sprintf("%s [%d symbols, %d lines]\n", e.Path, symCount, e.Lines))
		} else {
			sb.WriteString(fmt.Sprintf("%s [%d lines]\n", e.Path, e.Lines))
		}
	}
	return sb.String()
}
