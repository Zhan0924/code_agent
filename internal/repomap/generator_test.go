package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestGenerator_Generate_GoFile(t *testing.T) {
	dir := t.TempDir()
	// Create a Go source file
	goCode := `package main

type UserService struct {
	db *sql.DB
}

func NewUserService(db *sql.DB) *UserService {
	return &UserService{db: db}
}

func (s *UserService) GetUser(id string) (*User, error) {
	return nil, nil
}

type User struct {
	ID   string
	Name string
}

const MaxRetries = 3
`
	writeTestFile(t, dir, "service.go", goCode)

	gen := NewGenerator(zap.NewNop())
	content, err := gen.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Should contain public symbols
	if !strings.Contains(content, "UserService") {
		t.Error("expected UserService in repo map")
	}
	if !strings.Contains(content, "NewUserService") {
		t.Error("expected NewUserService in repo map")
	}
	if !strings.Contains(content, "User") {
		t.Error("expected User in repo map")
	}
	if !strings.Contains(content, "MaxRetries") {
		t.Error("expected MaxRetries in repo map")
	}
}

func TestGenerator_Generate_PythonFile(t *testing.T) {
	dir := t.TempDir()
	pyCode := `class AuthService:
    def __init__(self):
        pass

    def authenticate(self, token):
        pass

def create_app():
    pass

async def handle_request(req):
    pass

def _private_helper():
    pass
`
	writeTestFile(t, dir, "auth.py", pyCode)

	gen := NewGenerator(zap.NewNop())
	content, err := gen.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(content, "AuthService") {
		t.Error("expected AuthService")
	}
	if !strings.Contains(content, "create_app") {
		t.Error("expected create_app")
	}
	if !strings.Contains(content, "handle_request") {
		t.Error("expected handle_request")
	}
	// Private helper should NOT appear
	if strings.Contains(content, "_private_helper") {
		t.Error("private helper should be excluded")
	}
}

func TestGenerator_Generate_TypeScriptFile(t *testing.T) {
	dir := t.TempDir()
	tsCode := `export class Router {
  constructor() {}
}

export interface Config {
  port: number;
}

export type Handler = (req: Request) => Response;

export function createServer(config: Config) {
  return null;
}

export const DEFAULT_PORT = 3000;
`
	writeTestFile(t, dir, "server.ts", tsCode)

	gen := NewGenerator(zap.NewNop())
	content, err := gen.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(content, "Router") {
		t.Error("expected Router class")
	}
	if !strings.Contains(content, "Config") {
		t.Error("expected Config interface")
	}
	if !strings.Contains(content, "Handler") {
		t.Error("expected Handler type")
	}
	if !strings.Contains(content, "createServer") {
		t.Error("expected createServer func")
	}
}

func TestGenerator_Generate_RustFile(t *testing.T) {
	dir := t.TempDir()
	rsCode := `pub struct Config {
    pub port: u16,
}

pub enum Status {
    Active,
    Inactive,
}

pub trait Handler {
    fn handle(&self);
}

pub fn start_server(config: Config) {
    // ...
}

pub async fn listen(addr: &str) {
    // ...
}
`
	writeTestFile(t, dir, "main.rs", rsCode)

	gen := NewGenerator(zap.NewNop())
	content, err := gen.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(content, "Config") {
		t.Error("expected Config struct")
	}
	if !strings.Contains(content, "Status") {
		t.Error("expected Status enum")
	}
	if !strings.Contains(content, "Handler") {
		t.Error("expected Handler trait")
	}
	if !strings.Contains(content, "start_server") {
		t.Error("expected start_server func")
	}
}

func TestGenerator_SkipsDirs(t *testing.T) {
	dir := t.TempDir()
	// Create file in node_modules — should be skipped
	nmDir := filepath.Join(dir, "node_modules", "pkg")
	if err := os.MkdirAll(nmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "node_modules/pkg/index.js", "function InNodeModules() {}")
	// Create file in .git — should be skipped
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, ".git/config", "some config")
	// Create regular file
	writeTestFile(t, dir, "main.go", "package main\nfunc Main() {}\n")

	gen := NewGenerator(zap.NewNop())
	content, err := gen.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(content, "InNodeModules") {
		t.Error("should skip node_modules")
	}
	if !strings.Contains(content, "Main") {
		t.Error("should include main.go")
	}
}

func TestGenerator_Cache(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "main.go", "package main\nfunc Hello() {}\n")

	gen := NewGenerator(zap.NewNop())

	// First call generates
	c1, _ := gen.Generate(dir)
	// Second call should use cache
	c2, _ := gen.Generate(dir)
	if c1 != c2 {
		t.Error("cached result should be identical")
	}

	// Invalidate and regenerate
	gen.InvalidateCache(dir)
	c3, _ := gen.Generate(dir)
	// Content should be same (same files), but from fresh generation
	if !strings.Contains(c3, "Hello") {
		t.Error("regenerated map should still contain Hello")
	}
}

func TestGenerator_GetEntries(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "main.go", "package main\nfunc Run() {}\n")
	writeTestFile(t, dir, "util.py", "def helper(): pass\n")

	gen := NewGenerator(zap.NewNop())
	entries, err := gen.GetEntries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}
}

func TestFormatCompact(t *testing.T) {
	entries := []FileEntry{
		{Path: "main.go", Lines: 50, Symbols: []Symbol{{Kind: "func", Name: "Main"}}},
		{Path: "config.go", Lines: 20},
	}
	compact := FormatCompact(entries)
	if !strings.Contains(compact, "1 symbols") {
		t.Error("should show symbol count")
	}
	if !strings.Contains(compact, "config.go") {
		t.Error("should list config.go")
	}
}

func TestExtractSymbols_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.go")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	symbols, lines, err := extractSymbols(path, "go")
	if err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 0 {
		t.Errorf("expected 0 symbols, got %d", len(symbols))
	}
	if lines != 0 {
		t.Errorf("expected 0 lines, got %d", lines)
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func writeTestFile(t *testing.T, baseDir, relPath, content string) {
	t.Helper()
	absPath := filepath.Join(baseDir, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
