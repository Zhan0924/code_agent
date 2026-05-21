package rag

import (
	"testing"
)

func TestExtractGoDeps_BasicFile(t *testing.T) {
	src := `package main

import (
	"fmt"
	"os"
)

type Server struct {
	Logger *zap.Logger
	Config Config
}

func (s *Server) Start() error {
	fmt.Println("starting")
	return os.ErrNotExist
}

func NewServer(cfg Config) *Server {
	return &Server{Config: cfg}
}
`
	info := ExtractGoDeps("main.go", src)

	if info.Package != "main" {
		t.Errorf("expected package main, got %s", info.Package)
	}
	if len(info.Imports) != 2 {
		t.Errorf("expected 2 imports, got %d", len(info.Imports))
	}
	if len(info.Symbols) < 3 {
		t.Errorf("expected at least 3 symbols (Server, Server.Start, NewServer), got %d: %v", len(info.Symbols), info.Symbols)
	}

	// Should detect type references
	foundConfig := false
	for _, ref := range info.TypeRefs {
		if ref == "Config" {
			foundConfig = true
		}
	}
	if !foundConfig {
		t.Errorf("expected Config in type refs, got %v", info.TypeRefs)
	}
}

func TestExtractGoDeps_Embedding(t *testing.T) {
	src := `package svc

type BaseService struct {
	Name string
}

type UserService struct {
	BaseService
	repo UserRepo
}
`
	info := ExtractGoDeps("svc.go", src)

	if len(info.Embeds) == 0 {
		t.Fatal("expected at least one embed (BaseService)")
	}
	found := false
	for _, e := range info.Embeds {
		if e == "BaseService" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected BaseService in embeds, got %v", info.Embeds)
	}
}

func TestExtractGoDeps_InterfaceEmbed(t *testing.T) {
	src := `package io

type Reader interface {
	Read(p []byte) (n int, err error)
}

type ReadCloser interface {
	Reader
	Close() error
}
`
	info := ExtractGoDeps("io.go", src)

	if len(info.Implements) == 0 {
		t.Fatal("expected at least one interface embed (Reader)")
	}
	found := false
	for _, impl := range info.Implements {
		if impl == "Reader" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Reader in implements, got %v", info.Implements)
	}
}

func TestDepGraph_AddAndQuery(t *testing.T) {
	g := NewDepGraph()

	g.RegisterSymbol("a.go", "pkg.FuncA")
	g.RegisterSymbol("b.go", "pkg.FuncB")

	g.AddEdge(DepEdge{From: "pkg.FuncA", To: "pkg.FuncB", Kind: DepCall, FilePath: "a.go", Weight: 1.0})
	g.AddEdge(DepEdge{From: "pkg.FuncA", To: "Config", Kind: DepType, FilePath: "a.go", Weight: 0.6})

	deps := g.Dependencies("pkg.FuncA")
	if len(deps) != 2 {
		t.Errorf("expected 2 outgoing edges, got %d", len(deps))
	}

	dependents := g.Dependents("pkg.FuncB")
	if len(dependents) != 1 {
		t.Errorf("expected 1 incoming edge, got %d", len(dependents))
	}
	if dependents[0].From != "pkg.FuncA" {
		t.Errorf("expected dependent from pkg.FuncA, got %s", dependents[0].From)
	}
}

func TestDepGraph_ExpandRetrievalContext(t *testing.T) {
	g := NewDepGraph()

	g.RegisterSymbol("a.go", "A")
	g.RegisterSymbol("b.go", "B")
	g.RegisterSymbol("c.go", "C")
	g.RegisterSymbol("d.go", "D")

	// A → B → C, A → D
	g.AddEdge(DepEdge{From: "A", To: "B", Kind: DepCall, Weight: 1.0})
	g.AddEdge(DepEdge{From: "B", To: "C", Kind: DepCall, Weight: 1.0})
	g.AddEdge(DepEdge{From: "A", To: "D", Kind: DepType, Weight: 0.6})

	expanded := g.ExpandRetrievalContext([]string{"A"}, 2)

	if len(expanded) < 3 {
		t.Errorf("expected at least 3 expanded symbols (B, C, D), got %d: %v", len(expanded), expanded)
	}

	// B should have higher score than C (closer)
	scoreB, scoreC := 0.0, 0.0
	for _, s := range expanded {
		switch s.Symbol {
		case "B":
			scoreB = s.Score
		case "C":
			scoreC = s.Score
		}
	}
	if scoreB <= scoreC {
		t.Errorf("expected B score > C score (closer hop), got B=%.2f C=%.2f", scoreB, scoreC)
	}
}

func TestDepGraph_RemoveFile(t *testing.T) {
	g := NewDepGraph()

	g.RegisterSymbol("a.go", "A")
	g.RegisterSymbol("b.go", "B")
	g.AddEdge(DepEdge{From: "A", To: "B", Kind: DepCall, Weight: 1.0})

	g.RemoveFile("a.go")

	syms := g.SymbolsInFile("a.go")
	if len(syms) != 0 {
		t.Errorf("expected no symbols after removal, got %v", syms)
	}

	stats := g.Stats()
	if stats.Files != 1 {
		t.Errorf("expected 1 file remaining, got %d", stats.Files)
	}
}

func TestDepGraph_Stats(t *testing.T) {
	g := NewDepGraph()

	g.RegisterSymbol("a.go", "A")
	g.RegisterSymbol("a.go", "B")
	g.RegisterSymbol("b.go", "C")
	g.AddEdge(DepEdge{From: "A", To: "C", Kind: DepCall, Weight: 1.0})
	g.AddEdge(DepEdge{From: "B", To: "C", Kind: DepType, Weight: 0.6})

	stats := g.Stats()
	if stats.Files != 2 {
		t.Errorf("expected 2 files, got %d", stats.Files)
	}
	if stats.Symbols != 3 {
		t.Errorf("expected 3 symbols, got %d", stats.Symbols)
	}
	if stats.Edges != 2 {
		t.Errorf("expected 2 edges, got %d", stats.Edges)
	}
}

func TestPopulateDepGraph(t *testing.T) {
	src := `package svc

import "fmt"

type Handler struct {
	Logger Logger
}

func (h *Handler) Handle() {
	fmt.Println("handling")
}
`
	info := ExtractGoDeps("handler.go", src)
	g := NewDepGraph()
	PopulateDepGraph(g, info)

	stats := g.Stats()
	if stats.Symbols == 0 {
		t.Error("expected symbols to be registered")
	}
	if stats.Edges == 0 {
		t.Error("expected edges to be added")
	}
}

func TestQualifiedSymbol(t *testing.T) {
	cases := []struct {
		file, sym, want string
	}{
		{"internal/rag/engine.go", "Engine", "engine.Engine"},
		{"main.go", "main", "main.main"},
		{"", "Foo", "Foo"},
	}
	for _, tc := range cases {
		got := qualifiedSymbol(tc.file, tc.sym)
		if got != tc.want {
			t.Errorf("qualifiedSymbol(%q, %q) = %q, want %q", tc.file, tc.sym, got, tc.want)
		}
	}
}
