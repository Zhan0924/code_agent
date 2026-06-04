// Package llm — retryable.go classifies LLM call errors as retryable or
// terminal so the ReAct loop and any other retry harness can stop hammering
// the provider when the error is deterministic (401, 400, quota exhausted,
// etc.) instead of paying 3 × backoff to learn the same answer.
//
// Policy:
//   - OpenAI-compatible providers (go-openai): inspect *openai.APIError /
//     *openai.RequestError via errors.As; retry on 408 / 425 / 429 / 5xx,
//     reject all other 4xx.
//   - Anthropic SDK: errors are wrapped with %w in non-streaming and %v in
//     streaming (the latter is a separate TODO). We fall back to substring
//     matching on the error message for "429", "rate limit", "5xx" markers,
//     plus context.DeadlineExceeded / net.Error transient flag.
//   - Unknown error types: default to true (preserve prior unconditional
//     retry behaviour) so we do not regress on unfamiliar provider stacks.
package llm

import (
	"context"
	"errors"
	"net"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// IsRetryable returns true if a fresh attempt is plausibly worth the latency.
// Returning false instructs the caller to surface the error immediately.
//
// The classifier is intentionally conservative: when in doubt, retry. False
// is reserved for errors we are confident will not change on a retry within
// the next few seconds (auth failures, malformed requests, hard quota).
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Context cancellation: deadline exceeded is retryable (next attempt may
	// have more time); explicit cancel is not.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// go-openai typed errors carry HTTP status codes.
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return isRetryableStatus(apiErr.HTTPStatusCode)
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		// RequestError wraps transport-level failures (Err != nil) before
		// the API ever responded — those are uniformly retryable. When a
		// status is set (some transports populate it), reuse the matrix.
		if reqErr.HTTPStatusCode != 0 {
			return isRetryableStatus(reqErr.HTTPStatusCode)
		}
		return true
	}

	// net.Error: most transport failures are retryable; net.OpError, DNS,
	// EOF, broken pipe etc.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	// Anthropic SDK and any provider that wraps with %v lose the type, so
	// fall back to substring matching on common transient markers.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "429"),
		strings.Contains(msg, "rate limit"),
		strings.Contains(msg, "too many requests"),
		strings.Contains(msg, "overloaded"):
		return true
	case strings.Contains(msg, "500"),
		strings.Contains(msg, "502"),
		strings.Contains(msg, "503"),
		strings.Contains(msg, "504"),
		strings.Contains(msg, "internal server error"),
		strings.Contains(msg, "service unavailable"),
		strings.Contains(msg, "bad gateway"),
		strings.Contains(msg, "gateway timeout"):
		return true
	case strings.Contains(msg, "401"),
		strings.Contains(msg, "403"),
		strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "invalid_api_key"),
		strings.Contains(msg, "invalid api key"):
		return false
	case strings.Contains(msg, "400"),
		strings.Contains(msg, "invalid request"),
		strings.Contains(msg, "context length exceeded"),
		strings.Contains(msg, "max_tokens"):
		return false
	}

	// Default: unfamiliar error, prefer to retry rather than silently drop.
	return true
}

// isRetryableStatus encodes the HTTP status matrix. 408 (timeout), 425
// (too early), 429 (rate limit), and all 5xx are retryable; every other
// 4xx (and anything outside HTTP range) is treated as terminal.
func isRetryableStatus(code int) bool {
	switch code {
	case 408, 425, 429:
		return true
	}
	if code >= 500 && code <= 599 {
		return true
	}
	if code >= 400 && code <= 499 {
		return false
	}
	// 0, 1xx, 2xx, 3xx: not an error condition we recognise. Retry to be safe.
	return true
}
