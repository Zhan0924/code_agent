package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sony/gobreaker"
	"go.uber.org/zap"
)

// stubFailingStreamProvider is a Provider whose ChatCompletionStream always
// returns an error before any chunk is delivered (the "setup failure" path
// that the streaming RecordFailure fix targets).
type stubFailingStreamProvider struct {
	name string
	err  error
}

func (p *stubFailingStreamProvider) Name() string { return p.name }
func (p *stubFailingStreamProvider) ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	return nil, p.err
}
func (p *stubFailingStreamProvider) ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error) {
	return nil, p.err
}

// TestClient_StreamFailure_RecordsSharedBreaker pins the fix for the streaming
// RecordFailure asymmetry: a streaming setup failure must increment the shared
// counter just like a non-streaming failure does at client.go:238-239.
//
// Before the fix, a misbehaving provider could fail forever on the streaming
// code path without ever tripping siblings via Redis — only non-streaming
// traffic counted toward the trip decision.
func TestClient_StreamFailure_RecordsSharedBreaker(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	shared := NewSharedCircuitBreaker(SharedBreakerConfig{
		Rdb:       rdb,
		Prefix:    "test:breaker",
		Window:    30 * time.Second,
		Threshold: 1000, // high enough that Allow keeps returning true
	}, zap.NewNop())

	primary := &stubFailingStreamProvider{name: "stub-primary", err: errors.New("stream setup failed")}

	c := &Client{
		primary:       primary,
		fallback:      nil,
		breaker:       gobreaker.NewCircuitBreaker(gobreaker.Settings{Name: "test"}),
		sharedBreaker: shared,
		logger:        zap.NewNop(),
	}

	ctx := context.Background()

	// Sanity: no failure recorded yet.
	keyBefore := shared.keyFor(primary.Name())
	if mr.Exists(keyBefore) {
		t.Fatalf("expected no breaker key before call, got one at %s", keyBefore)
	}

	_, callErr := c.ChatCompletionStream(ctx, &ChatRequest{})
	if callErr == nil {
		t.Fatal("expected ChatCompletionStream to return an error from stub provider")
	}

	keyAfter := shared.keyFor(primary.Name())
	gotStr, err := mr.Get(keyAfter)
	if err != nil {
		t.Fatalf("expected breaker counter at %s after failed stream call, got error: %v", keyAfter, err)
	}
	if gotStr != "1" {
		t.Fatalf("expected breaker counter = 1, got %s", gotStr)
	}
}

// TestClient_StreamFailure_NoSharedBreakerOK exercises the nil-shared-breaker
// path: when SharedCircuitBreaker is not wired (deployments without Redis-backed
// shared state), the streaming failure path must still return the underlying
// error and must not panic on the nil receiver.
func TestClient_StreamFailure_NoSharedBreakerOK(t *testing.T) {
	primary := &stubFailingStreamProvider{name: "stub-primary", err: errors.New("boom")}
	c := &Client{
		primary:       primary,
		breaker:       gobreaker.NewCircuitBreaker(gobreaker.Settings{Name: "test"}),
		sharedBreaker: nil,
		logger:        zap.NewNop(),
	}

	if _, err := c.ChatCompletionStream(context.Background(), &ChatRequest{}); err == nil {
		t.Fatal("expected error, got nil")
	}
}
