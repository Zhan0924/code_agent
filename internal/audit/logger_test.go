package audit

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestNewLogger(t *testing.T) {
	core, _ := observer.New(zap.InfoLevel)
	base := zap.New(core)
	l := NewLogger(base)
	if l == nil {
		t.Fatal("expected non-nil Logger")
	}
}

func TestLogEvent(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	base := zap.New(core)
	l := NewLogger(base)

	l.Log(context.Background(), Event{
		Type:      EventSandboxExecution,
		SessionID: "sess-123",
		TaskID:    "task-456",
		Action:    "execute_code",
		Details:   map[string]string{"language": "python"},
		Success:   true,
	})

	if logs.Len() != 1 {
		t.Fatalf("expected 1 log entry, got %d", logs.Len())
	}
	entry := logs.All()[0]
	if entry.Message != "audit_event" {
		t.Errorf("expected message 'audit_event', got %q", entry.Message)
	}

	// Check that structured fields are present via ContextMap
	cm := entry.ContextMap()
	if v, ok := cm["event_type"]; !ok || v != "sandbox_execution" {
		t.Errorf("expected event_type 'sandbox_execution', got %v", cm["event_type"])
	}
	if v, ok := cm["session_id"]; !ok || v != "sess-123" {
		t.Errorf("expected session_id 'sess-123', got %v", cm["session_id"])
	}
}

func TestLogApproval(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	base := zap.New(core)
	l := NewLogger(base)

	l.LogApproval(context.Background(), EventApprovalGranted, "task-1", "sess-1", "deploy", true)

	if logs.Len() != 1 {
		t.Fatalf("expected 1 log, got %d", logs.Len())
	}
}

func TestLogMCPCall(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	base := zap.New(core)
	l := NewLogger(base)

	l.LogMCPCall(context.Background(), "github-mcp", "list_prs", true, "")

	if logs.Len() != 1 {
		t.Fatalf("expected 1 log, got %d", logs.Len())
	}
}

func TestLogSandboxExec(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	base := zap.New(core)
	l := NewLogger(base)

	l.LogSandboxExec(context.Background(), "sess-1", "python", 0, 2*time.Second)

	if logs.Len() != 1 {
		t.Fatalf("expected 1 log, got %d", logs.Len())
	}
}

func TestEventTimestampAutoSet(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	base := zap.New(core)
	l := NewLogger(base)

	before := time.Now()
	l.Log(context.Background(), Event{Type: EventSessionCreated, Action: "create"})
	after := time.Now()

	if logs.Len() != 1 {
		t.Fatal("expected 1 log entry")
	}

	// Verify timestamp was auto-set
	entry := logs.All()[0]
	for _, f := range entry.Context {
		if f.Key == "event_time" {
			ts := time.Unix(0, f.Integer)
			if ts.Before(before) || ts.After(after) {
				t.Errorf("auto-set timestamp %v not in expected range [%v, %v]", ts, before, after)
			}
			return
		}
	}
}
