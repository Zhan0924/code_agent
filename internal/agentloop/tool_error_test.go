package agentloop

import (
	"errors"
	"testing"

	"github.com/agent/code_agent/internal/models"
)

func TestClassifyToolError_NotFound(t *testing.T) {
	result := &models.ToolResult{Content: "Error: no such file or directory: /tmp/foo.txt", IsError: true}
	te := ClassifyToolError("read_file", result, nil)
	if te.Category != ErrCatNotFound {
		t.Errorf("expected not_found, got %s", te.Category)
	}
	if !te.Retryable {
		t.Error("expected retryable")
	}
}

func TestClassifyToolError_InvalidArgs(t *testing.T) {
	te := ClassifyToolError("edit_file", nil, errors.New("invalid JSON: missing required field 'path'"))
	if te.Category != ErrCatInvalidArgs {
		t.Errorf("expected invalid_args, got %s", te.Category)
	}
}

func TestClassifyToolError_Permission(t *testing.T) {
	result := &models.ToolResult{Content: "permission denied: /etc/shadow", IsError: true}
	te := ClassifyToolError("read_file", result, nil)
	if te.Category != ErrCatPermission {
		t.Errorf("expected permission, got %s", te.Category)
	}
	if te.Retryable {
		t.Error("permission errors should not be retryable")
	}
}

func TestClassifyToolError_Timeout(t *testing.T) {
	te := ClassifyToolError("run_command", nil, errors.New("context deadline exceeded"))
	if te.Category != ErrCatTimeout {
		t.Errorf("expected timeout, got %s", te.Category)
	}
}

func TestClassifyToolError_ExecFailed(t *testing.T) {
	result := &models.ToolResult{Content: "❌ Command FAILED with exit status 1\ncompilation error", IsError: true}
	te := ClassifyToolError("run_command", result, nil)
	if te.Category != ErrCatExecFailed {
		t.Errorf("expected exec_failed, got %s", te.Category)
	}
}

func TestClassifyToolError_Internal(t *testing.T) {
	te := ClassifyToolError("unknown", nil, errors.New("something completely unexpected"))
	if te.Category != ErrCatInternal {
		t.Errorf("expected internal, got %s", te.Category)
	}
	if te.Retryable {
		t.Error("internal errors should not be retryable")
	}
}
