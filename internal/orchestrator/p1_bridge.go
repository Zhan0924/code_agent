package orchestrator

import (
	"context"

	"github.com/agent/code_agent/internal/lsp"
	"github.com/agent/code_agent/internal/pty"
	"github.com/agent/code_agent/internal/treesitter"
	"go.uber.org/zap"
)

// PTYManager is the interface for persistent PTY session management.
type PTYManager interface {
	GetOrCreate(ctx context.Context, workspaceID string) (pty.ShellSession, error)
	Create(ctx context.Context, workspaceID, name string) (pty.ShellSession, error)
	ActiveSessions(workspaceID string) []pty.SessionInfo
	DestroyAll(workspaceID string) error
	Close() error
}

// LSPClient is the interface for LSP operations.
type LSPClient interface {
	Initialize(ctx context.Context, language, rootPath string) error
	GotoDefinition(ctx context.Context, uri string, line, col int) ([]lsp.Location, error)
	FindReferences(ctx context.Context, uri string, line, col int) ([]lsp.Location, error)
	Rename(ctx context.Context, uri string, line, col int, newName string) (*lsp.WorkspaceEdit, error)
	Hover(ctx context.Context, uri string, line, col int) (*lsp.HoverResult, error)
	ShutdownAll() error
}

// TreeSitterParser is the interface for AST parsing.
type TreeSitterParser interface {
	ExtractSymbols(language, content string) ([]treesitter.Symbol, error)
	ChunkByAST(language, content string) ([]treesitter.Chunk, error)
	SupportedLanguages() []string
}

// SetPTYManager injects an optional PTY session manager for persistent shell execution.
func (o *Orchestrator) SetPTYManager(m PTYManager) {
	o.ptyManager = m
	if m != nil && o.toolRegistry != nil {
		if err := o.RegisterPTYTools(o.toolRegistry); err != nil {
			o.logger.Error("failed to register PTY tools", zap.Error(err))
		}
	}
}

// SetLSPClient injects an optional LSP client for type-aware code intelligence.
func (o *Orchestrator) SetLSPClient(c LSPClient) {
	o.lspClient = c
	if c != nil && o.toolRegistry != nil {
		if err := o.RegisterLSPTools(o.toolRegistry); err != nil {
			o.logger.Error("failed to register LSP tools", zap.Error(err))
		}
	}
}

// SetTreeSitterParser injects an optional tree-sitter parser for AST analysis.
func (o *Orchestrator) SetTreeSitterParser(p TreeSitterParser) {
	o.tsParser = p
}
