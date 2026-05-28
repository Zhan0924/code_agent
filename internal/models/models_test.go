package models

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMessage_JSONRoundTrip(t *testing.T) {
	msg := Message{
		ID:        "msg-1",
		Role:      RoleAssistant,
		Content:   "Hello, world!",
		Timestamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		ToolCalls: []ToolCall{
			{ID: "tc-1", Name: "read_file", Args: json.RawMessage(`{"path":"main.go"}`)},
		},
		CacheControl: &CacheControl{Type: "ephemeral"},
		Pinned:       true,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.ID != msg.ID {
		t.Errorf("ID = %q, want %q", got.ID, msg.ID)
	}
	if got.Role != msg.Role {
		t.Errorf("Role = %q, want %q", got.Role, msg.Role)
	}
	if got.Content != msg.Content {
		t.Errorf("Content = %q, want %q", got.Content, msg.Content)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Name != "read_file" {
		t.Errorf("ToolCall.Name = %q, want read_file", got.ToolCalls[0].Name)
	}
	if got.CacheControl == nil || got.CacheControl.Type != "ephemeral" {
		t.Errorf("CacheControl mismatch")
	}
	if !got.Pinned {
		t.Error("Pinned should be true")
	}
}

func TestToolCall_JSONRoundTrip(t *testing.T) {
	tc := ToolCall{
		ID:   "call-123",
		Name: "run_tests",
		Args: json.RawMessage(`{"path":"./...","verbose":true}`),
	}

	data, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ToolCall
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.ID != tc.ID || got.Name != tc.Name {
		t.Errorf("ToolCall mismatch: got %+v", got)
	}
	if string(got.Args) != string(tc.Args) {
		t.Errorf("Args = %s, want %s", got.Args, tc.Args)
	}
}

func TestToolResult_JSONRoundTrip(t *testing.T) {
	tr := ToolResult{
		ToolCallID: "call-123",
		Content:    "PASS\n",
		IsError:    false,
	}

	data, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ToolResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.ToolCallID != tr.ToolCallID {
		t.Errorf("ToolCallID = %q, want %q", got.ToolCallID, tr.ToolCallID)
	}
	if got.IsError != false {
		t.Error("IsError should be false")
	}
}

func TestReactStreamEvent_JSONRoundTrip(t *testing.T) {
	ev := ReactStreamEvent{
		Type:       "tool_call",
		Step:       2,
		MaxSteps:   10,
		ToolName:   "read_file",
		ToolArgs:   `{"path":"main.go"}`,
		ToolCallID: "tc-1",
		TaskID:     "task-1",
	}

	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ReactStreamEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Type != ev.Type {
		t.Errorf("Type = %q, want %q", got.Type, ev.Type)
	}
	if got.Step != 2 || got.MaxSteps != 10 {
		t.Errorf("Step/MaxSteps mismatch: %d/%d", got.Step, got.MaxSteps)
	}
	if got.ToolName != "read_file" {
		t.Errorf("ToolName = %q", got.ToolName)
	}
}

func TestTaskState_Constants(t *testing.T) {
	states := []TaskState{
		TaskStatePending, TaskStatePlanning, TaskStateExecuting,
		TaskStateSuspended, TaskStateCompleted, TaskStateFailed, TaskStateCancelled,
	}
	seen := make(map[TaskState]bool)
	for _, s := range states {
		if seen[s] {
			t.Errorf("duplicate state: %q", s)
		}
		seen[s] = true
		if s == "" {
			t.Error("empty state constant")
		}
	}
}

func TestStreamEvent_JSONRoundTrip(t *testing.T) {
	ev := StreamEvent{
		Type:   "message",
		Data:   json.RawMessage(`{"text":"hello"}`),
		TaskID: "task-42",
	}

	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got StreamEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Type != "message" || got.TaskID != "task-42" {
		t.Errorf("StreamEvent mismatch: %+v", got)
	}
}

func TestMessage_OmitEmpty(t *testing.T) {
	msg := Message{
		ID:      "msg-1",
		Role:    RoleUser,
		Content: "hi",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]interface{}
	json.Unmarshal(data, &raw)

	if _, ok := raw["tool_calls"]; ok {
		t.Error("tool_calls should be omitted when empty")
	}
	if _, ok := raw["cache_control"]; ok {
		t.Error("cache_control should be omitted when nil")
	}
}
