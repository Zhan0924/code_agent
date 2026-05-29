// Package treesitter provides multi-language AST parsing using tree-sitter.
// It supports 10+ languages and provides symbol extraction and AST-based chunking
// for RAG and repomap subsystems.
//
// Build with -tags tree_sitter to enable CGO-based tree-sitter parsing.
// Without the tag, a fallback implementation using regex heuristics is used.
package treesitter

// Symbol represents a code symbol extracted from AST.
type Symbol struct {
	Name       string
	Kind       string // "function", "method", "class", "interface", "struct", "variable", "type"
	Signature  string
	StartLine  int
	EndLine    int
	Parent     string // enclosing class/struct name
	Visibility string // "public", "private"
}

// Chunk represents an AST-based code chunk for RAG indexing.
type Chunk struct {
	SymbolName   string
	SymbolType   string
	Content      string
	StartLine    int
	EndLine      int
	Dependencies []string
	Signature    string
}

// Parser provides multi-language AST parsing capabilities.
type Parser interface {
	// ExtractSymbols extracts all symbols from source code.
	ExtractSymbols(language, content string) ([]Symbol, error)
	// ChunkByAST splits source code into semantically meaningful chunks.
	ChunkByAST(language, content string) ([]Chunk, error)
	// SupportedLanguages returns the list of supported language identifiers.
	SupportedLanguages() []string
}
