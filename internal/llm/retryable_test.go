package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// TestIsRetryable covers the five canonical buckets called out in the plan:
// 401 (auth — terminal), 429 (rate limit — retry), 500 (server — retry),
// context.DeadlineExceeded (transport timeout — retry), unknown error type
// (default conservative — retry).
func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context_canceled", context.Canceled, false},
		{"context_deadline", context.DeadlineExceeded, true},

		// go-openai APIError, typed extraction.
		{"openai_401", &openai.APIError{HTTPStatusCode: 401, Message: "unauthorized"}, false},
		{"openai_400", &openai.APIError{HTTPStatusCode: 400, Message: "bad request"}, false},
		{"openai_403", &openai.APIError{HTTPStatusCode: 403, Message: "forbidden"}, false},
		{"openai_408", &openai.APIError{HTTPStatusCode: 408, Message: "timeout"}, true},
		{"openai_429", &openai.APIError{HTTPStatusCode: 429, Message: "rate limit"}, true},
		{"openai_500", &openai.APIError{HTTPStatusCode: 500, Message: "server error"}, true},
		{"openai_503", &openai.APIError{HTTPStatusCode: 503, Message: "unavailable"}, true},

		// go-openai RequestError, transport-layer wrapping.
		{"openai_request_transport", &openai.RequestError{Err: errors.New("connection reset")}, true},
		{"openai_request_401", &openai.RequestError{HTTPStatusCode: 401, Err: errors.New("auth")}, false},

		// net.Error path: a fake transport-level error.
		{"net_error", &net.OpError{Op: "dial", Err: errors.New("no route")}, true},

		// Anthropic / fmt.Errorf %w wrapping path: string match.
		{"wrapped_429", fmt.Errorf("anthropic: %w", errors.New("429 Too Many Requests")), true},
		{"wrapped_500", fmt.Errorf("anthropic: %w", errors.New("500 Internal Server Error")), true},
		{"wrapped_401", fmt.Errorf("anthropic: %w", errors.New("401 Unauthorized")), false},
		{"wrapped_overloaded", fmt.Errorf("anthropic: %w", errors.New("model overloaded, please retry")), true},
		{"wrapped_invalid_api_key", fmt.Errorf("anthropic: %w", errors.New("invalid_api_key")), false},
		{"wrapped_context_length", fmt.Errorf("anthropic: %w", errors.New("context length exceeded for model")), false},

		// Unknown error type, no markers → conservative retry.
		{"unknown_default", errors.New("something opaque happened"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRetryable(tt.err)
			if got != tt.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsRetryable_StatusCodeMatrix pins the HTTP status decision matrix so a
// future refactor cannot silently drop a code from the retryable set.
func TestIsRetryable_StatusCodeMatrix(t *testing.T) {
	cases := map[int]bool{
		200: true,  // not a real error path, defaults to retry
		400: false,
		401: false,
		403: false,
		404: false,
		408: true,
		425: true,
		429: true,
		500: true,
		502: true,
		503: true,
		504: true,
		599: true,
	}
	for code, want := range cases {
		got := isRetryableStatus(code)
		if got != want {
			t.Errorf("isRetryableStatus(%d) = %v, want %v", code, got, want)
		}
	}
}
