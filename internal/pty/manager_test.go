package pty

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestLocalSession_BasicExecution(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tmpDir := t.TempDir()
	mgr, err := NewManager(ManagerConfig{
		Backend:       "local",
		WorkspaceBase: tmpDir,
		MaxSessions:   3,
		IdleTimeout:   5 * time.Minute,
		OutputLimit:   1048576,
		Shell:         "/bin/bash",
		Timeout:       10 * time.Second,
	}, logger)
	require.NoError(t, err)
	defer mgr.Close()

	ctx := context.Background()

	// Create workspace dir
	os.MkdirAll(tmpDir+"/test-ws", 0o755)

	sess, err := mgr.GetOrCreate(ctx, "test-ws")
	require.NoError(t, err)
	assert.True(t, sess.IsAlive())

	// Test simple command
	result, err := sess.Execute(ctx, "echo hello")
	require.NoError(t, err)
	assert.Contains(t, result.Output, "hello")
	assert.Equal(t, 0, result.ExitCode)
}

func TestLocalSession_StatePersistence(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tmpDir := t.TempDir()
	mgr, err := NewManager(ManagerConfig{
		Backend:       "local",
		WorkspaceBase: tmpDir,
		MaxSessions:   3,
		IdleTimeout:   5 * time.Minute,
		OutputLimit:   1048576,
		Shell:         "/bin/bash",
		Timeout:       10 * time.Second,
	}, logger)
	require.NoError(t, err)
	defer mgr.Close()

	ctx := context.Background()
	os.MkdirAll(tmpDir+"/test-ws", 0o755)

	sess, err := mgr.GetOrCreate(ctx, "test-ws")
	require.NoError(t, err)

	// Set environment variable
	result, err := sess.Execute(ctx, "export MY_VAR=test_value")
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	// Verify it persists
	result, err = sess.Execute(ctx, "echo $MY_VAR")
	require.NoError(t, err)
	assert.Contains(t, result.Output, "test_value")

	// Test cd persistence
	result, err = sess.Execute(ctx, "mkdir -p /tmp/pty-test-dir && cd /tmp/pty-test-dir")
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	result, err = sess.Execute(ctx, "pwd")
	require.NoError(t, err)
	assert.Contains(t, result.Output, "/tmp/pty-test-dir")
}

func TestLocalSession_ExitCode(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tmpDir := t.TempDir()
	mgr, err := NewManager(ManagerConfig{
		Backend:       "local",
		WorkspaceBase: tmpDir,
		MaxSessions:   3,
		IdleTimeout:   5 * time.Minute,
		OutputLimit:   1048576,
		Shell:         "/bin/bash",
		Timeout:       10 * time.Second,
	}, logger)
	require.NoError(t, err)
	defer mgr.Close()

	ctx := context.Background()
	os.MkdirAll(tmpDir+"/test-ws", 0o755)

	sess, err := mgr.GetOrCreate(ctx, "test-ws")
	require.NoError(t, err)

	// Successful command
	result, err := sess.Execute(ctx, "true")
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	// Failing command
	result, err = sess.Execute(ctx, "false")
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExitCode)
}

func TestManager_MaxSessions(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tmpDir := t.TempDir()
	mgr, err := NewManager(ManagerConfig{
		Backend:       "local",
		WorkspaceBase: tmpDir,
		MaxSessions:   2,
		IdleTimeout:   5 * time.Minute,
		OutputLimit:   1048576,
		Shell:         "/bin/bash",
		Timeout:       10 * time.Second,
	}, logger)
	require.NoError(t, err)
	defer mgr.Close()

	ctx := context.Background()
	os.MkdirAll(tmpDir+"/test-ws", 0o755)

	_, err = mgr.Create(ctx, "test-ws", "session1")
	require.NoError(t, err)

	_, err = mgr.Create(ctx, "test-ws", "session2")
	require.NoError(t, err)

	_, err = mgr.Create(ctx, "test-ws", "session3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max sessions limit")
}

func TestManager_DestroyAll(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tmpDir := t.TempDir()
	mgr, err := NewManager(ManagerConfig{
		Backend:       "local",
		WorkspaceBase: tmpDir,
		MaxSessions:   3,
		IdleTimeout:   5 * time.Minute,
		OutputLimit:   1048576,
		Shell:         "/bin/bash",
		Timeout:       10 * time.Second,
	}, logger)
	require.NoError(t, err)
	defer mgr.Close()

	ctx := context.Background()
	os.MkdirAll(tmpDir+"/test-ws", 0o755)

	_, err = mgr.Create(ctx, "test-ws", "s1")
	require.NoError(t, err)
	_, err = mgr.Create(ctx, "test-ws", "s2")
	require.NoError(t, err)

	assert.Len(t, mgr.ActiveSessions("test-ws"), 2)

	err = mgr.DestroyAll("test-ws")
	require.NoError(t, err)

	assert.Len(t, mgr.ActiveSessions("test-ws"), 0)
}

func TestManager_GetOrCreate_ReturnsSame(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tmpDir := t.TempDir()
	mgr, err := NewManager(ManagerConfig{
		Backend:       "local",
		WorkspaceBase: tmpDir,
		MaxSessions:   3,
		IdleTimeout:   5 * time.Minute,
		OutputLimit:   1048576,
		Shell:         "/bin/bash",
		Timeout:       10 * time.Second,
	}, logger)
	require.NoError(t, err)
	defer mgr.Close()

	ctx := context.Background()
	os.MkdirAll(tmpDir+"/test-ws", 0o755)

	s1, err := mgr.GetOrCreate(ctx, "test-ws")
	require.NoError(t, err)

	s2, err := mgr.GetOrCreate(ctx, "test-ws")
	require.NoError(t, err)

	assert.Equal(t, s1.ID(), s2.ID())
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "hello"},
		{"\x1b[32mgreen\x1b[0m", "green"},
		{"\x1b[1;31mred bold\x1b[0m text", "red bold text"},
		{"no escapes here", "no escapes here"},
	}

	for _, tt := range tests {
		result := stripANSI(tt.input)
		assert.Equal(t, tt.expected, result, "input: %q", tt.input)
	}
}

func TestLocalSession_OutputTruncation(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tmpDir := t.TempDir()
	mgr, err := NewManager(ManagerConfig{
		Backend:       "local",
		WorkspaceBase: tmpDir,
		MaxSessions:   3,
		IdleTimeout:   5 * time.Minute,
		OutputLimit:   100, // Very small limit
		Shell:         "/bin/bash",
		Timeout:       10 * time.Second,
	}, logger)
	require.NoError(t, err)
	defer mgr.Close()

	ctx := context.Background()
	os.MkdirAll(tmpDir+"/test-ws", 0o755)

	sess, err := mgr.GetOrCreate(ctx, "test-ws")
	require.NoError(t, err)

	// Generate lots of output
	result, err := sess.Execute(ctx, "seq 1 1000")
	require.NoError(t, err)
	assert.True(t, result.Truncated)
	assert.True(t, len(result.Output) <= 200) // Some slack for line buffering
}

func TestManager_InvalidBackend(t *testing.T) {
	logger := zaptest.NewLogger(t)
	_, err := NewManager(ManagerConfig{
		Backend: "invalid",
	}, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported backend")
}

func TestLocalSession_MultilineOutput(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tmpDir := t.TempDir()
	mgr, err := NewManager(ManagerConfig{
		Backend:       "local",
		WorkspaceBase: tmpDir,
		MaxSessions:   3,
		IdleTimeout:   5 * time.Minute,
		OutputLimit:   1048576,
		Shell:         "/bin/bash",
		Timeout:       10 * time.Second,
	}, logger)
	require.NoError(t, err)
	defer mgr.Close()

	ctx := context.Background()
	os.MkdirAll(tmpDir+"/test-ws", 0o755)

	sess, err := mgr.GetOrCreate(ctx, "test-ws")
	require.NoError(t, err)

	result, err := sess.Execute(ctx, "echo line1 && echo line2 && echo line3")
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)

	lines := strings.Split(strings.TrimSpace(result.Output), "\n")
	hasLine1, hasLine2, hasLine3 := false, false, false
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "line1" {
			hasLine1 = true
		}
		if l == "line2" {
			hasLine2 = true
		}
		if l == "line3" {
			hasLine3 = true
		}
	}
	assert.True(t, hasLine1, "should contain line1")
	assert.True(t, hasLine2, "should contain line2")
	assert.True(t, hasLine3, "should contain line3")
}
