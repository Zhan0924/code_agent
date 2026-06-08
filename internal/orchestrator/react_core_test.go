// react_core_test.go — validates the LLM-call progress instrumentation
// (callLLMWithProgress).  In production this wraps every ChatCompletion so
// the front-end keeps seeing heartbeat events even when finalize blocks for
// 20+ minutes on a non-streaming completion.  The test reproduces that by
// injecting a deliberately slow stub call and a shortened progress interval.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
)

type recordingSink struct {
	mu     sync.Mutex
	events []models.ReactStreamEvent
}

func (r *recordingSink) Emit(ev models.ReactStreamEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingSink) typesByOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Type
	}
	return out
}

func (r *recordingSink) count(typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func (r *recordingSink) first(typ string) (models.ReactStreamEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == typ {
			return e, true
		}
	}
	return models.ReactStreamEvent{}, false
}

// TestCallLLMWithProgress_EmitsProgressDuringSlowCall — golden path:
// when ChatCompletion takes meaningfully longer than the heartbeat interval,
// we must see started → ≥1 progress(with elapsed_ms>0) → completed.
func TestCallLLMWithProgress_EmitsProgressDuringSlowCall(t *testing.T) {
	prev := llmProgressInterval
	llmProgressInterval = 50 * time.Millisecond
	defer func() { llmProgressInterval = prev }()

	sink := &recordingSink{}
	slowCall := func(ctx context.Context) (*llm.ChatResponse, error) {
		// Sleep long enough to guarantee multiple ticks (≥ 4 intervals).
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		return &llm.ChatResponse{Content: "ok"}, nil
	}

	resp, err := callLLMWithProgress(context.Background(), slowCall, sink, "task-x", 1, 0, 5, 3)
	if err != nil {
		t.Fatalf("call returned error: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("expected response content=ok, got %+v", resp)
	}

	types := sink.typesByOrder()
	if len(types) < 3 {
		t.Fatalf("expected ≥3 events, got %d (%v)", len(types), types)
	}
	if types[0] != "llm_call_started" {
		t.Fatalf("first event must be llm_call_started, got %q (%v)", types[0], types)
	}
	if types[len(types)-1] != "llm_call_completed" {
		t.Fatalf("last event must be llm_call_completed, got %q (%v)", types[len(types)-1], types)
	}
	if got := sink.count("llm_call_progress"); got < 1 {
		t.Fatalf("expected ≥1 llm_call_progress event during 300ms call @ 50ms interval, got %d (events=%v)", got, types)
	}

	// llm_call_started Content must contain attempt/messages/tools fields.
	started, _ := sink.first("llm_call_started")
	var startedPayload struct {
		Attempt  int `json:"attempt"`
		Messages int `json:"messages"`
		Tools    int `json:"tools"`
	}
	if jerr := json.Unmarshal([]byte(started.Content), &startedPayload); jerr != nil {
		t.Fatalf("llm_call_started Content not valid JSON: %v (raw=%q)", jerr, started.Content)
	}
	if startedPayload.Attempt != 1 || startedPayload.Messages != 5 || startedPayload.Tools != 3 {
		t.Fatalf("started payload mismatch: %+v", startedPayload)
	}

	// Every llm_call_progress event must carry elapsed_ms > 0.
	sink.mu.Lock()
	for i, e := range sink.events {
		if e.Type != "llm_call_progress" {
			continue
		}
		var p struct {
			Attempt   int   `json:"attempt"`
			ElapsedMS int64 `json:"elapsed_ms"`
		}
		if jerr := json.Unmarshal([]byte(e.Content), &p); jerr != nil {
			sink.mu.Unlock()
			t.Fatalf("progress event #%d Content invalid JSON: %v (raw=%q)", i, jerr, e.Content)
		}
		if p.ElapsedMS <= 0 {
			sink.mu.Unlock()
			t.Fatalf("progress event #%d expected elapsed_ms>0, got %d", i, p.ElapsedMS)
		}
		if p.Attempt != 1 {
			sink.mu.Unlock()
			t.Fatalf("progress event #%d expected attempt=1, got %d", i, p.Attempt)
		}
	}
	sink.mu.Unlock()

	// llm_call_completed must report err=false on success path.
	completed, _ := sink.first("llm_call_completed")
	if !strings.Contains(completed.Content, `"err":false`) {
		t.Fatalf("completed event missing err:false, got %q", completed.Content)
	}
}

// TestCallLLMWithProgress_FastCallSkipsProgress — when the LLM returns
// before the first tick, we still emit started+completed (telemetry sentinel)
// but should NOT emit a stray progress event (would race the cancel).
func TestCallLLMWithProgress_FastCallSkipsProgress(t *testing.T) {
	prev := llmProgressInterval
	llmProgressInterval = 500 * time.Millisecond
	defer func() { llmProgressInterval = prev }()

	sink := &recordingSink{}
	fastCall := func(ctx context.Context) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{Content: "fast"}, nil
	}
	if _, err := callLLMWithProgress(context.Background(), fastCall, sink, "task-y", 2, 1, 1, 0); err != nil {
		t.Fatalf("fast call errored: %v", err)
	}
	types := sink.typesByOrder()
	if len(types) != 2 {
		t.Fatalf("expected exactly started+completed, got %v", types)
	}
	if types[0] != "llm_call_started" || types[1] != "llm_call_completed" {
		t.Fatalf("expected [llm_call_started, llm_call_completed], got %v", types)
	}
}

// TestCallLLMWithProgress_PropagatesErrorAndMarksErrTrue — on LLM failure,
// completed.Content must report `err:true`, and the underlying error must
// surface back to the caller.
func TestCallLLMWithProgress_PropagatesErrorAndMarksErrTrue(t *testing.T) {
	prev := llmProgressInterval
	llmProgressInterval = 5 * time.Second
	defer func() { llmProgressInterval = prev }()

	wantErr := errors.New("llm boom")
	sink := &recordingSink{}
	failingCall := func(ctx context.Context) (*llm.ChatResponse, error) {
		return nil, wantErr
	}
	resp, err := callLLMWithProgress(context.Background(), failingCall, sink, "task-z", 3, 2, 2, 1)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped %v, got %v", wantErr, err)
	}
	if resp != nil {
		t.Fatalf("expected nil response on error, got %+v", resp)
	}
	completed, ok := sink.first("llm_call_completed")
	if !ok {
		t.Fatalf("expected llm_call_completed event, got %v", sink.typesByOrder())
	}
	if !strings.Contains(completed.Content, `"err":true`) {
		t.Fatalf("completed event must report err:true on failure, got %q", completed.Content)
	}
}
