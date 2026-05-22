package agentloop

import (
	"strings"

	"github.com/agent/code_agent/internal/models"
)

// ToolErrorCategory classifies tool execution failures.
type ToolErrorCategory string

const (
	ErrCatInvalidArgs ToolErrorCategory = "invalid_args"
	ErrCatNotFound    ToolErrorCategory = "not_found"
	ErrCatPermission  ToolErrorCategory = "permission"
	ErrCatTimeout     ToolErrorCategory = "timeout"
	ErrCatExecFailed  ToolErrorCategory = "exec_failed"
	ErrCatInternal    ToolErrorCategory = "internal"
)

// ToolError represents a classified tool execution failure.
type ToolError struct {
	Category   ToolErrorCategory
	ToolName   string
	Message    string
	Retryable  bool
	Suggestion string
}

// ClassifyToolError categorizes a tool failure based on error content.
func ClassifyToolError(toolName string, result *models.ToolResult, err error) *ToolError {
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if result != nil {
		msg = result.Content
	}

	lower := strings.ToLower(msg)

	te := &ToolError{
		ToolName: toolName,
		Message:  msg,
	}

	switch {
	case containsAny(lower, "no such file", "not found", "does not exist", "no file", "cannot find"):
		te.Category = ErrCatNotFound
		te.Retryable = true
		te.Suggestion = "资源不存在。先用 list/search 确认正确路径后重试"
	case containsAny(lower, "invalid", "missing required", "parse error", "unknown flag", "unexpected token"):
		te.Category = ErrCatInvalidArgs
		te.Retryable = true
		te.Suggestion = "参数有误。检查参数格式后重试"
	case containsAny(lower, "permission denied", "access denied", "forbidden", "unauthorized"):
		te.Category = ErrCatPermission
		te.Retryable = false
		te.Suggestion = "权限不足，不要重试此工具。寻找替代方案"
	case containsAny(lower, "timeout", "deadline exceeded", "context deadline"):
		te.Category = ErrCatTimeout
		te.Retryable = true
		te.Suggestion = "操作超时，可重试一次"
	case containsAny(lower, "❌ command failed", "exit status", "exec failed", "command not found"):
		te.Category = ErrCatExecFailed
		te.Retryable = true
		te.Suggestion = "命令失败。分析错误输出，修改命令或代码后重试"
	default:
		te.Category = ErrCatInternal
		te.Retryable = false
		te.Suggestion = "内部错误，不应重试。换用其他方法"
	}

	return te
}

func containsAny(s string, patterns ...string) bool {
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
