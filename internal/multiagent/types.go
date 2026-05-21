package multiagent

import (
	"context"
	"encoding/json"
	"time"
)

// AgentType defines the specialization of a sub-agent.
type AgentType string

const (
	AgentCode   AgentType = "code"
	AgentTest   AgentType = "test"
	AgentReview AgentType = "review"
)

// AgentResult holds the output of a sub-agent's work.
type AgentResult struct {
	AgentID     string        `json:"agent_id"`
	AgentType   AgentType     `json:"agent_type"`
	StepID      string        `json:"step_id"`
	Output      string        `json:"output"`
	Success     bool          `json:"success"`
	Error       string        `json:"error,omitempty"`
	Duration    time.Duration `json:"duration"`
	Artifacts   []string      `json:"artifacts,omitempty"`
}

// DelegationRequest describes work to be delegated to a sub-agent.
type DelegationRequest struct {
	StepID       string          `json:"step_id"`
	AgentType    AgentType       `json:"agent_type"`
	Action       string          `json:"action"`
	Task         string          `json:"task"`
	Parameters   json.RawMessage `json:"parameters,omitempty"`
	Context      string          `json:"context,omitempty"`
	Timeout      time.Duration   `json:"timeout,omitempty"`
	AllowedTools []string        `json:"allowed_tools,omitempty"`
}

// SupervisorConfig configures the multi-agent supervisor.
type SupervisorConfig struct {
	MaxParallel   int           `json:"max_parallel"`
	StepTimeout   time.Duration `json:"step_timeout"`
	TotalTimeout  time.Duration `json:"total_timeout"`
}

// DefaultSupervisorConfig returns sensible defaults.
func DefaultSupervisorConfig() SupervisorConfig {
	return SupervisorConfig{
		MaxParallel:  3,
		StepTimeout:  5 * time.Minute,
		TotalTimeout: 30 * time.Minute,
	}
}

// ToolDispatcher is the interface sub-agents use to execute tools.
type ToolDispatcher interface {
	Dispatch(ctx context.Context, toolName string, args json.RawMessage) (string, error)
}

// Message represents inter-agent communication.
type Message struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Type      string    `json:"type"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}
