package rag

import (
	"strings"
	"sync"
)

// DepKind classifies the type of dependency relationship.
type DepKind string

const (
	DepCall      DepKind = "call"      // function/method call
	DepImport    DepKind = "import"    // package import
	DepType      DepKind = "type"      // type reference (field type, param type, return type)
	DepImplement DepKind = "implement" // interface implementation
	DepEmbed     DepKind = "embed"     // struct embedding
)

// DepEdge represents a directed dependency from one symbol to another.
type DepEdge struct {
	From     string  `json:"from"`      // source symbol (file:symbol or package.Symbol)
	To       string  `json:"to"`        // target symbol
	Kind     DepKind `json:"kind"`
	FilePath string  `json:"file_path"` // file where the dependency originates
	Weight   float64 `json:"weight"`    // relevance weight (1.0 = direct call, 0.5 = type ref)
}

// DepGraph is a concurrent-safe directed graph of code dependencies.
type DepGraph struct {
	mu       sync.RWMutex
	outgoing map[string][]DepEdge // symbol → edges going out
	incoming map[string][]DepEdge // symbol → edges coming in
	fileSyms map[string][]string  // file_path → symbols defined in that file
}

// NewDepGraph creates an empty dependency graph.
func NewDepGraph() *DepGraph {
	return &DepGraph{
		outgoing: make(map[string][]DepEdge),
		incoming: make(map[string][]DepEdge),
		fileSyms: make(map[string][]string),
	}
}

// AddEdge inserts a dependency edge into the graph.
func (g *DepGraph) AddEdge(edge DepEdge) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.outgoing[edge.From] = append(g.outgoing[edge.From], edge)
	g.incoming[edge.To] = append(g.incoming[edge.To], edge)
}

// RegisterSymbol associates a symbol with its defining file.
func (g *DepGraph) RegisterSymbol(filePath, symbol string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fileSyms[filePath] = append(g.fileSyms[filePath], symbol)
}

// Dependents returns symbols that depend on the given symbol (incoming edges).
func (g *DepGraph) Dependents(symbol string) []DepEdge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.incoming[symbol]
}

// Dependencies returns symbols that the given symbol depends on (outgoing edges).
func (g *DepGraph) Dependencies(symbol string) []DepEdge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.outgoing[symbol]
}

// SymbolsInFile returns all symbols defined in a file.
func (g *DepGraph) SymbolsInFile(filePath string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.fileSyms[filePath]
}

// ExpandRetrievalContext takes a set of retrieved symbols and expands them
// by following dependency edges up to maxDepth hops. Returns additional
// symbols that should be included in the context, with decaying relevance.
func (g *DepGraph) ExpandRetrievalContext(seeds []string, maxDepth int) []ScoredSymbol {
	g.mu.RLock()
	defer g.mu.RUnlock()

	visited := make(map[string]bool, len(seeds))
	for _, s := range seeds {
		visited[s] = true
	}

	var expanded []ScoredSymbol
	frontier := seeds
	decay := 1.0

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		decay *= 0.5
		var nextFrontier []string

		for _, sym := range frontier {
			for _, edge := range g.outgoing[sym] {
				if visited[edge.To] {
					continue
				}
				visited[edge.To] = true
				score := edge.Weight * decay
				expanded = append(expanded, ScoredSymbol{Symbol: edge.To, Score: score, Kind: edge.Kind})
				nextFrontier = append(nextFrontier, edge.To)
			}
			for _, edge := range g.incoming[sym] {
				if visited[edge.From] {
					continue
				}
				visited[edge.From] = true
				score := edge.Weight * decay * 0.7 // incoming edges slightly less relevant
				expanded = append(expanded, ScoredSymbol{Symbol: edge.From, Score: score, Kind: edge.Kind})
				nextFrontier = append(nextFrontier, edge.From)
			}
		}
		frontier = nextFrontier
	}

	return expanded
}

// ScoredSymbol is a symbol with a relevance score from graph expansion.
type ScoredSymbol struct {
	Symbol string  `json:"symbol"`
	Score  float64 `json:"score"`
	Kind   DepKind `json:"kind"`
}

// RemoveFile removes all edges and symbols associated with a file.
func (g *DepGraph) RemoveFile(filePath string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	syms := g.fileSyms[filePath]
	delete(g.fileSyms, filePath)

	for _, sym := range syms {
		delete(g.outgoing, sym)
		delete(g.incoming, sym)
	}

	// Clean edges referencing removed symbols
	symSet := make(map[string]bool, len(syms))
	for _, s := range syms {
		symSet[s] = true
	}
	for key, edges := range g.outgoing {
		filtered := edges[:0]
		for _, e := range edges {
			if !symSet[e.To] {
				filtered = append(filtered, e)
			}
		}
		g.outgoing[key] = filtered
	}
	for key, edges := range g.incoming {
		filtered := edges[:0]
		for _, e := range edges {
			if !symSet[e.From] {
				filtered = append(filtered, e)
			}
		}
		g.incoming[key] = filtered
	}
}

// Stats returns graph statistics.
func (g *DepGraph) Stats() DepGraphStats {
	g.mu.RLock()
	defer g.mu.RUnlock()

	totalEdges := 0
	for _, edges := range g.outgoing {
		totalEdges += len(edges)
	}

	totalSymbols := 0
	for _, syms := range g.fileSyms {
		totalSymbols += len(syms)
	}

	return DepGraphStats{
		Files:   len(g.fileSyms),
		Symbols: totalSymbols,
		Edges:   totalEdges,
	}
}

// DepGraphStats holds summary statistics for the dependency graph.
type DepGraphStats struct {
	Files   int `json:"files"`
	Symbols int `json:"symbols"`
	Edges   int `json:"edges"`
}

// qualifiedSymbol builds a qualified symbol name from file path and symbol name.
func qualifiedSymbol(filePath, symbolName string) string {
	if filePath == "" {
		return symbolName
	}
	// Use package-style qualification: strip extension, use last path component
	parts := strings.Split(filePath, "/")
	pkg := parts[len(parts)-1]
	if idx := strings.LastIndex(pkg, "."); idx > 0 {
		pkg = pkg[:idx]
	}
	return pkg + "." + symbolName
}
