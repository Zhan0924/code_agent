package lsp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestNewClient(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := Config{
		Servers: map[string]ServerConfig{
			"go": {Command: "gopls", Args: []string{"serve"}},
		},
		Timeout: 10,
	}

	c := NewClient(cfg, logger)
	require.NotNil(t, c)
}

func TestClient_InitializeShutdown(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := Config{
		Servers: map[string]ServerConfig{
			"go": {Command: "gopls", Args: []string{"serve"}},
		},
		Timeout: 10,
	}

	c := NewClient(cfg, logger)
	ctx := context.Background()

	err := c.Initialize(ctx, "go", "/tmp/test")
	require.NoError(t, err)

	// Double initialize should be idempotent
	err = c.Initialize(ctx, "go", "/tmp/test")
	require.NoError(t, err)

	err = c.Shutdown("go")
	require.NoError(t, err)

	// Shutdown non-existent is fine
	err = c.Shutdown("python")
	require.NoError(t, err)
}

func TestClient_ShutdownAll(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := Config{
		Servers: map[string]ServerConfig{
			"go":     {Command: "gopls", Args: []string{"serve"}},
			"python": {Command: "pylsp"},
		},
		Timeout: 10,
	}

	c := NewClient(cfg, logger)
	ctx := context.Background()

	c.Initialize(ctx, "go", "/tmp/test")
	c.Initialize(ctx, "python", "/tmp/test")

	err := c.ShutdownAll()
	require.NoError(t, err)
}

func TestClient_MethodsReturnNotImplemented(t *testing.T) {
	logger := zaptest.NewLogger(t)
	c := NewClient(Config{}, logger)
	ctx := context.Background()

	_, err := c.GotoDefinition(ctx, "file:///test.go", 1, 1)
	assert.Error(t, err)

	_, err = c.FindReferences(ctx, "file:///test.go", 1, 1)
	assert.Error(t, err)

	_, err = c.Rename(ctx, "file:///test.go", 1, 1, "newName")
	assert.Error(t, err)

	_, err = c.Hover(ctx, "file:///test.go", 1, 1)
	assert.Error(t, err)

	_, err = c.DocumentSymbols(ctx, "file:///test.go")
	assert.Error(t, err)

	// DidChange should succeed (no-op)
	err = c.DidChange(ctx, "file:///test.go", "content")
	assert.NoError(t, err)
}
