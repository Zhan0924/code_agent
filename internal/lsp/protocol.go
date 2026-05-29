// Package lsp implements a Language Server Protocol client for type-aware
// code intelligence. It manages LSP server processes per language and
// provides semantic operations (go-to-definition, find-references, etc.)
// to the orchestrator's tool system.
package lsp

// Location represents a source code location.
type Location struct {
	URI       string
	StartLine int
	StartCol  int
	EndLine   int
	EndCol    int
}

// Range represents a text range in a document.
type Range struct {
	Start Position
	End   Position
}

// Position represents a position in a document.
type Position struct {
	Line      int
	Character int
}

// TextEdit represents a text edit to be applied to a document.
type TextEdit struct {
	Range   Range
	NewText string
}

// SymbolInfo represents symbol information from the LSP.
type SymbolInfo struct {
	Name       string
	Kind       int
	Range      Range
	Children   []SymbolInfo
	Detail     string
	Deprecated bool
}

// HoverResult holds the result of a hover request.
type HoverResult struct {
	Contents string
	Range    *Range
}

// WorkspaceEdit represents a set of edits across files.
type WorkspaceEdit struct {
	Changes map[string][]TextEdit // URI -> edits
}
