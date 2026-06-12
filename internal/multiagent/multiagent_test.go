package multiagent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/planner"
	"go.uber.org/zap"
)

type mockDispatcher struct {
	mu    sync.Mutex
	calls []string
}

func (m *mockDispatcher) Dispatch(_ context.Context, toolName string, _ json.RawMessage) (string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, toolName)
	m.mu.Unlock()
	return "ok: " + toolName, nil
}

func TestAgentPool_AcquireRelease(t *testing.T) {
	pool := NewAgentPool(2, zap.NewNop())

	a1 := pool.Acquire(AgentCode)
	if a1 == nil {
		t.Fatal("expected non-nil agent")
	}
	if a1.Type != AgentCode {
		t.Errorf("expected AgentCode, got %s", a1.Type)
	}

	a2 := pool.Acquire(AgentTest)
	if a2.Type != AgentTest {
		t.Errorf("expected AgentTest, got %s", a2.Type)
	}

	pool.Release(a1)
	pool.Release(a2)

	if pool.Size() != 2 {
		t.Errorf("expected pool size 2, got %d", pool.Size())
	}

	// Re-acquire should reuse pooled agent
	a3 := pool.Acquire(AgentCode)
	if a3.ID != a1.ID {
		t.Errorf("expected reused agent %s, got %s", a1.ID, a3.ID)
	}
	pool.Release(a3)
}

func TestSubAgent_Execute(t *testing.T) {
	d := &mockDispatcher{}
	agent := NewSubAgent(AgentCode, zap.NewNop())

	params := json.RawMessage(`{"tool":"` + models.ToolReadFile + `","args":{"path":"main.go"}}`)
	output, err := agent.Execute(context.Background(), DelegationRequest{
		StepID:     "step-1",
		AgentType:  AgentCode,
		Task:       "read a file",
		Parameters: params,
	}, d)

	if err != nil {
		t.Fatal(err)
	}
	if output != "ok: read_file" {
		t.Errorf("unexpected output: %s", output)
	}
	if len(d.calls) != 1 || d.calls[0] != models.ToolReadFile {
		t.Errorf("unexpected dispatch calls: %v", d.calls)
	}
}

func TestSubAgent_Execute_UsesActionFallback(t *testing.T) {
	d := &mockDispatcher{}
	agent := NewSubAgent(AgentCode, zap.NewNop())

	params := json.RawMessage(`{"path":"main.go"}`)
	output, err := agent.Execute(context.Background(), DelegationRequest{
		StepID:     "step-1",
		AgentType:  AgentCode,
		Action:     models.ToolReadFile,
		Task:       "read a file",
		Parameters: params,
	}, d)

	if err != nil {
		t.Fatal(err)
	}
	if output != "ok: read_file" {
		t.Errorf("unexpected output: %s", output)
	}
	if len(d.calls) != 1 || d.calls[0] != models.ToolReadFile {
		t.Errorf("unexpected dispatch calls: %v", d.calls)
	}
}

func TestSubAgent_DisallowedTool(t *testing.T) {
	d := &mockDispatcher{}
	agent := NewSubAgent(AgentReview, zap.NewNop())

	params := json.RawMessage(`{"tool":"` + models.ToolWriteFile + `","args":{}}`)
	_, err := agent.Execute(context.Background(), DelegationRequest{
		StepID:     "step-1",
		AgentType:  AgentReview,
		Task:       "write something",
		Parameters: params,
	}, d)

	if err == nil {
		t.Fatal("expected error for disallowed tool")
	}
}

func TestSupervisor_Execute(t *testing.T) {
	d := &mockDispatcher{}
	cfg := DefaultSupervisorConfig()
	sup := NewSupervisor(d, cfg, zap.NewNop())

	plan := &planner.Plan{
		Steps: []planner.Step{
			{ID: "s1", Action: models.ToolReadFile, Description: "read main", DependsOn: nil},
			{ID: "s2", Action: models.ToolReadFile, Description: "read util", DependsOn: nil},
			{ID: "s3", Action: models.ToolWriteFile, Description: "write output", DependsOn: []string{"s1", "s2"}},
		},
	}

	result, err := sup.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Errorf("expected success, got failure: %s", result.Summary)
	}
	if len(result.Results) != 3 {
		t.Errorf("expected 3 results, got %d", len(result.Results))
	}
}

func TestMessageBus_PubSub(t *testing.T) {
	bus := NewMessageBus(zap.NewNop())
	ch := bus.Subscribe("agent-1")

	bus.Publish(Message{From: "supervisor", To: "agent-1", Type: "task", Content: "do work"})

	select {
	case msg := <-ch:
		if msg.Content != "do work" {
			t.Errorf("unexpected content: %s", msg.Content)
		}
	default:
		t.Fatal("expected message on channel")
	}

	bus.Unsubscribe("agent-1", ch)
}
