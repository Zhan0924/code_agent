package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"go.uber.org/zap"
)

// SubAgent is a lightweight agent specialized for a specific task type.
// It wraps a ToolDispatcher with a restricted tool set and focused system prompt.
type SubAgent struct {
	ID        string    `json:"id"`
	Type      AgentType `json:"type"`
	logger    *zap.Logger
}

var agentCounter atomic.Uint64

// NewSubAgent creates a new sub-agent instance.
func NewSubAgent(agentType AgentType, logger *zap.Logger) *SubAgent {
	id := fmt.Sprintf("%s-%d", agentType, agentCounter.Add(1))
	return &SubAgent{
		ID:     id,
		Type:   agentType,
		logger: logger.With(zap.String("agent_id", id)),
	}
}

// Execute runs a delegation request using the provided tool dispatcher.
func (a *SubAgent) Execute(ctx context.Context, req DelegationRequest, dispatcher ToolDispatcher) (string, error) {
	a.logger.Info("executing task",
		zap.String("step_id", req.StepID),
		zap.String("task", req.Task))

	allowedTools := a.allowedTools()
	if len(req.AllowedTools) > 0 {
		allowedTools = req.AllowedTools
	}

	if len(req.Parameters) > 0 {
		var params struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		}
		if json.Unmarshal(req.Parameters, &params) == nil && params.Tool != "" {
			if !isAllowed(params.Tool, allowedTools) {
				return "", fmt.Errorf("tool %q not allowed for agent type %s", params.Tool, a.Type)
			}
			return dispatcher.Dispatch(ctx, params.Tool, params.Args)
		}
	}

	// Default: dispatch using the step's action as the tool name
	toolName := req.Action
	if toolName == "" {
		return "", fmt.Errorf("no action specified in delegation request for step %s", req.StepID)
	}
	if !isAllowed(toolName, allowedTools) {
		return "", fmt.Errorf("tool %q not allowed for agent type %s", toolName, a.Type)
	}
	return dispatcher.Dispatch(ctx, toolName, req.Parameters)
}

func (a *SubAgent) allowedTools() []string {
	switch a.Type {
	case AgentCode:
		return []string{"read_file", "write_file", "edit_file", "patch_file",
			"list_files", "create_directory", "git_status", "git_diff",
			"git_commit", "git_branch", "git_log", "run_workspace_cmd"}
	case AgentTest:
		return []string{"run_tests", "execute_code", "read_file", "run_workspace_cmd"}
	case AgentReview:
		return []string{"read_file", "search_code", "list_files", "git_diff", "git_log"}
	default:
		return []string{"read_file", "search_code"}
	}
}

func isAllowed(tool string, allowed []string) bool {
	for _, t := range allowed {
		if t == tool {
			return true
		}
	}
	return false
}
