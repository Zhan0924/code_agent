package lsp

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// requireGopls skips the test unless a real gopls binary is on PATH, since the
// initialize/shutdown tests now spawn an actual LSP subprocess.
func requireGopls(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not found on PATH; skipping LSP subprocess test")
	}
}

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
	requireGopls(t)
	logger := zaptest.NewLogger(t)
	cfg := Config{
		Servers: map[string]ServerConfig{
			"go": {Command: "gopls", Languages: []string{".go"}},
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
	requireGopls(t)
	logger := zaptest.NewLogger(t)
	cfg := Config{
		Servers: map[string]ServerConfig{
			"go":     {Command: "gopls", Languages: []string{".go"}},
			"python": {Command: "pylsp", Languages: []string{".py"}},
		},
		Timeout: 10,
	}

	c := NewClient(cfg, logger)
	ctx := context.Background()

	_ = c.Initialize(ctx, "go", "/tmp/test")
	_ = c.Initialize(ctx, "python", "/tmp/test") // may fail if pylsp missing; ignored

	err := c.ShutdownAll()
	require.NoError(t, err)
}

func TestClient_MethodsErrorWhenNoServer(t *testing.T) {
	logger := zaptest.NewLogger(t)
	c := NewClient(Config{}, logger)
	ctx := context.Background()

	// With no server running, every semantic op must fail fast rather than
	// silently returning empty results (which would mask a misconfiguration).
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

	// DidChange resolves a connection too, so it also errors with no server.
	err = c.DidChange(ctx, "file:///test.go", "content")
	assert.Error(t, err)
}

func TestClient_URIHelpers(t *testing.T) {
	assert.Equal(t, "file:///a/b.go", fileToURI("/a/b.go"))
	assert.Equal(t, "file:///a/b.go", fileToURI("file:///a/b.go"))
	assert.Equal(t, "/a/b.go", uriToFile("file:///a/b.go"))
}

func TestDecodeLocations(t *testing.T) {
	// Single-object shape.
	single := decodeLocations([]byte(`{"uri":"file:///x.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}`))
	require.Len(t, single, 1)
	assert.Equal(t, "file:///x.go", single[0].URI)
	assert.Equal(t, 1, single[0].StartLine)
	assert.Equal(t, 2, single[0].StartCol)

	// Array shape.
	arr := decodeLocations([]byte(`[{"uri":"file:///y.go","range":{"start":{"line":3,"character":0},"end":{"line":3,"character":4}}}]`))
	require.Len(t, arr, 1)
	assert.Equal(t, "file:///y.go", arr[0].URI)

	// null / empty.
	assert.Nil(t, decodeLocations([]byte(`null`)))
	assert.Nil(t, decodeLocations(nil))
}

func TestDecodeSymbols(t *testing.T) {
	// Hierarchical DocumentSymbol[] shape.
	hier := decodeSymbols([]byte(`[{"name":"Foo","kind":12,"range":{"start":{"line":0,"character":0},"end":{"line":5,"character":0}},"children":[{"name":"bar","kind":6,"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":3}}}]}]`))
	require.Len(t, hier, 1)
	assert.Equal(t, "Foo", hier[0].Name)
	require.Len(t, hier[0].Children, 1)
	assert.Equal(t, "bar", hier[0].Children[0].Name)

	// Flat SymbolInformation[] shape.
	flat := decodeSymbols([]byte(`[{"name":"Baz","kind":5,"location":{"uri":"file:///z.go","range":{"start":{"line":2,"character":0},"end":{"line":2,"character":3}}}}]`))
	require.Len(t, flat, 1)
	assert.Equal(t, "Baz", flat[0].Name)

	assert.Nil(t, decodeSymbols([]byte(`null`)))
}
