package lsp

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"
)

// Client manages LSP server connections and provides semantic code operations.
type Client interface {
	// Initialize starts an LSP server for the given language and workspace.
	Initialize(ctx context.Context, language, rootPath string) error
	// Shutdown stops the LSP server for a language.
	Shutdown(language string) error
	// GotoDefinition finds the definition of a symbol.
	GotoDefinition(ctx context.Context, uri string, line, col int) ([]Location, error)
	// FindReferences finds all references to a symbol.
	FindReferences(ctx context.Context, uri string, line, col int) ([]Location, error)
	// Rename performs a semantic rename across the workspace.
	Rename(ctx context.Context, uri string, line, col int, newName string) (*WorkspaceEdit, error)
	// Hover gets hover information for a symbol.
	Hover(ctx context.Context, uri string, line, col int) (*HoverResult, error)
	// DocumentSymbols gets all symbols in a document.
	DocumentSymbols(ctx context.Context, uri string) ([]SymbolInfo, error)
	// DidChange notifies the server of document changes.
	DidChange(ctx context.Context, uri, content string) error
	// ShutdownAll stops all LSP servers.
	ShutdownAll() error
}

// Config holds LSP client configuration.
type Config struct {
	Servers map[string]ServerConfig
	Timeout int // seconds
}

// ServerConfig defines an LSP server for a language.
type ServerConfig struct {
	Command   string
	Args      []string
	Languages []string
}

type client struct {
	cfg     Config
	logger  *zap.Logger
	mu      sync.RWMutex
	servers map[string]*serverConn // language -> connection
}

type serverConn struct {
	language string
	// TODO: actual process management and JSON-RPC communication
	// This would reuse patterns from internal/mcp/client.go
}

// NewClient creates a new LSP client.
func NewClient(cfg Config, logger *zap.Logger) Client {
	return &client{
		cfg:     cfg,
		logger:  logger.With(zap.String("component", "lsp-client")),
		servers: make(map[string]*serverConn),
	}
}

func (c *client) Initialize(ctx context.Context, language, rootPath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.servers[language]; exists {
		return nil // already initialized
	}

	// TODO: Start LSP server process, send initialize request
	// For now, return a placeholder
	c.logger.Info("LSP server initialized (placeholder)", zap.String("language", language))
	c.servers[language] = &serverConn{language: language}
	return nil
}

func (c *client) Shutdown(language string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn, ok := c.servers[language]
	if !ok {
		return nil
	}

	// TODO: Send shutdown + exit to LSP server
	delete(c.servers, language)
	c.logger.Info("LSP server shutdown", zap.String("language", language))
	_ = conn
	return nil
}

func (c *client) ShutdownAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for lang := range c.servers {
		// TODO: graceful shutdown
		delete(c.servers, lang)
	}
	return nil
}

func (c *client) GotoDefinition(ctx context.Context, uri string, line, col int) ([]Location, error) {
	// TODO: Send textDocument/definition request
	return nil, fmt.Errorf("not implemented")
}

func (c *client) FindReferences(ctx context.Context, uri string, line, col int) ([]Location, error) {
	// TODO: Send textDocument/references request
	return nil, fmt.Errorf("not implemented")
}

func (c *client) Rename(ctx context.Context, uri string, line, col int, newName string) (*WorkspaceEdit, error) {
	// TODO: Send textDocument/rename request
	return nil, fmt.Errorf("not implemented")
}

func (c *client) Hover(ctx context.Context, uri string, line, col int) (*HoverResult, error) {
	// TODO: Send textDocument/hover request
	return nil, fmt.Errorf("not implemented")
}

func (c *client) DocumentSymbols(ctx context.Context, uri string) ([]SymbolInfo, error) {
	// TODO: Send textDocument/documentSymbol request
	return nil, fmt.Errorf("not implemented")
}

func (c *client) DidChange(ctx context.Context, uri, content string) error {
	// TODO: Send textDocument/didChange notification
	return nil
}
