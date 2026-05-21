// Package errors provides structured, typed errors for the Code Agent system.
// These enable programmatic error handling at the API layer (4xx vs 5xx classification)
// and consistent error responses across all subsystems.
package errors

import (
	"fmt"
	"net/http"
)

// Code represents a machine-readable error classification.
type Code string

const (
	CodeNotFound        Code = "NOT_FOUND"
	CodeInvalidInput    Code = "INVALID_INPUT"
	CodeUnauthorized    Code = "UNAUTHORIZED"
	CodeForbidden       Code = "FORBIDDEN"
	CodeConflict        Code = "CONFLICT"
	CodeRateLimited     Code = "RATE_LIMITED"
	CodeTimeout         Code = "TIMEOUT"
	CodeUnavailable     Code = "UNAVAILABLE"
	CodeInternal        Code = "INTERNAL"
	CodeLLMFailure      Code = "LLM_FAILURE"
	CodeSandboxFailure  Code = "SANDBOX_FAILURE"
	CodeRAGFailure      Code = "RAG_FAILURE"
	CodeMCPFailure      Code = "MCP_FAILURE"
	CodeApprovalPending Code = "APPROVAL_PENDING"
	CodeApprovalDenied  Code = "APPROVAL_DENIED"
)

// AgentError is the structured error type used throughout the system.
type AgentError struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	Cause   error  `json:"-"`
}

func (e *AgentError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

func (e *AgentError) Unwrap() error {
	return e.Cause
}

// HTTPStatus maps the error code to an HTTP status code.
func (e *AgentError) HTTPStatus() int {
	switch e.Code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeInvalidInput:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeConflict:
		return http.StatusConflict
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeTimeout:
		return http.StatusGatewayTimeout
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeApprovalPending:
		return http.StatusAccepted
	case CodeApprovalDenied:
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// ─── Constructor Helpers ─────────────────────────────────────────────────────

func New(code Code, message string) *AgentError {
	return &AgentError{Code: code, Message: message}
}

func Wrap(code Code, message string, cause error) *AgentError {
	return &AgentError{Code: code, Message: message, Cause: cause}
}

func NotFound(resource, id string) *AgentError {
	return &AgentError{
		Code:    CodeNotFound,
		Message: fmt.Sprintf("%s not found: %s", resource, id),
	}
}

func InvalidInput(detail string) *AgentError {
	return &AgentError{Code: CodeInvalidInput, Message: "invalid input", Detail: detail}
}

func Unavailable(service string, cause error) *AgentError {
	return &AgentError{
		Code:    CodeUnavailable,
		Message: fmt.Sprintf("%s is unavailable", service),
		Cause:   cause,
	}
}

func LLMFailure(cause error) *AgentError {
	return &AgentError{Code: CodeLLMFailure, Message: "LLM request failed", Cause: cause}
}

func SandboxFailure(cause error) *AgentError {
	return &AgentError{Code: CodeSandboxFailure, Message: "sandbox execution failed", Cause: cause}
}

func RateLimited() *AgentError {
	return &AgentError{Code: CodeRateLimited, Message: "rate limit exceeded"}
}

// IsCode checks if an error has a specific AgentError code.
func IsCode(err error, code Code) bool {
	if ae, ok := err.(*AgentError); ok {
		return ae.Code == code
	}
	return false
}
