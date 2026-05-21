package errors

import (
	"fmt"
	"net/http"
	"testing"
)

func TestAgentError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *AgentError
		want string
	}{
		{
			name: "without cause",
			err:  New(CodeNotFound, "session not found"),
			want: "[NOT_FOUND] session not found",
		},
		{
			name: "with cause",
			err:  Wrap(CodeLLMFailure, "LLM request failed", fmt.Errorf("timeout")),
			want: "[LLM_FAILURE] LLM request failed: timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentError_HTTPStatus(t *testing.T) {
	tests := []struct {
		code Code
		want int
	}{
		{CodeNotFound, http.StatusNotFound},
		{CodeInvalidInput, http.StatusBadRequest},
		{CodeUnauthorized, http.StatusUnauthorized},
		{CodeRateLimited, http.StatusTooManyRequests},
		{CodeTimeout, http.StatusGatewayTimeout},
		{CodeUnavailable, http.StatusServiceUnavailable},
		{CodeInternal, http.StatusInternalServerError},
		{CodeApprovalPending, http.StatusAccepted},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			err := New(tt.code, "test")
			if got := err.HTTPStatus(); got != tt.want {
				t.Errorf("HTTPStatus() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsCode(t *testing.T) {
	err := NotFound("session", "abc123")
	if !IsCode(err, CodeNotFound) {
		t.Error("IsCode should return true for matching code")
	}
	if IsCode(err, CodeInternal) {
		t.Error("IsCode should return false for non-matching code")
	}
	if IsCode(fmt.Errorf("plain error"), CodeNotFound) {
		t.Error("IsCode should return false for non-AgentError")
	}
}

func TestUnwrap(t *testing.T) {
	cause := fmt.Errorf("original error")
	err := Wrap(CodeInternal, "wrapped", cause)
	if err.Unwrap() != cause {
		t.Error("Unwrap should return the original cause")
	}
}

func TestConstructors(t *testing.T) {
	t.Run("InvalidInput", func(t *testing.T) {
		err := InvalidInput("name is required")
		if err.Code != CodeInvalidInput {
			t.Errorf("expected CodeInvalidInput, got %s", err.Code)
		}
		if err.Detail != "name is required" {
			t.Errorf("expected detail 'name is required', got %q", err.Detail)
		}
	})

	t.Run("Unavailable", func(t *testing.T) {
		err := Unavailable("redis", fmt.Errorf("conn refused"))
		if err.Code != CodeUnavailable {
			t.Errorf("expected CodeUnavailable, got %s", err.Code)
		}
	})

	t.Run("RateLimited", func(t *testing.T) {
		err := RateLimited()
		if err.HTTPStatus() != http.StatusTooManyRequests {
			t.Errorf("expected 429, got %d", err.HTTPStatus())
		}
	})
}
