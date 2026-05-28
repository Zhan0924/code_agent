package orchestrator

import "context"

// ProgressCallback is called by tool handlers to emit intermediate output
// (e.g., stdout lines from a running command) before the final ToolResult.
// The chunk parameter is a single line or small block of text.
type ProgressCallback func(chunk string)

type progressCtxKey struct{}

// WithProgressCallback stores a ProgressCallback in the context.
func WithProgressCallback(ctx context.Context, cb ProgressCallback) context.Context {
	return context.WithValue(ctx, progressCtxKey{}, cb)
}

// GetProgressCallback extracts the ProgressCallback from context, or nil if none.
func GetProgressCallback(ctx context.Context) ProgressCallback {
	cb, _ := ctx.Value(progressCtxKey{}).(ProgressCallback)
	return cb
}
