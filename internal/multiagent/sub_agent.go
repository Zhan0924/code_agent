package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// SubAgent is a lightweight agent specialized for a specific task type.
// It supports two execution modes:
//   - Direct dispatch: single tool call via ToolDispatcher (fast path)
//   - ReAct loop: multi-step reasoning via agentloop.Runner (reasoning path)
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

// Execute runs a delegation request. If AgentDeps is provided and reasoning is
// required, it uses a ReAct loop. Otherwise it falls back to direct dispatch.
func (a *SubAgent) Execute(ctx context.Context, req DelegationRequest, dispatcher ToolDispatcher) (string, error) {
	return a.ExecuteWithDeps(ctx, req, dispatcher, nil)
}

// ExecuteWithDeps runs a delegation request with optional ReAct dependencies.
func (a *SubAgent) ExecuteWithDeps(ctx context.Context, req DelegationRequest, dispatcher ToolDispatcher, deps *AgentDeps) (string, error) {
	a.logger.Info("executing task",
		zap.String("step_id", req.StepID),
		zap.String("task", req.Task),
		zap.Bool("reasoning", req.ReasoningRequired))

	// ReAct path: use agentloop.Runner for multi-step reasoning
	if req.ReasoningRequired && deps != nil && deps.LLM != nil {
		return a.executeReAct(ctx, req, deps)
	}

	// Fast path: direct single-tool dispatch
	return a.dispatchDirect(ctx, req, dispatcher)
}

func (a *SubAgent) executeReAct(ctx context.Context, req DelegationRequest, deps *AgentDeps) (string, error) {
	allowedTools := a.allowedTools()
	if len(req.AllowedTools) > 0 {
		allowedTools = req.AllowedTools
	}

	// Build filtered tool executor and provider
	toolExec := NewFilteredToolExecutor(deps.ToolExecutor, allowedTools)
	toolProv := NewFilteredToolProvider(deps.ToolProvider, allowedTools)

	sink := deps.EventSink
	if sink == nil {
		sink = agentloop.NoopSink{}
	}

	runner := agentloop.NewRunner(deps.LLM, toolExec, toolProv, agentloop.DefaultSubAgentConfig(), a.logger)

	systemPrompt := SubAgentSystemPrompt(a.Type, req.Task, req.Context)
	messages := []models.Message{
		{Role: models.RoleSystem, Content: systemPrompt},
		{Role: models.RoleUser, Content: buildUserMessage(req)},
	}

	result := runner.Run(ctx, agentloop.RunOpts{
		Messages: messages,
		TaskID:   req.StepID,
	}, sink)

	if result.HitStepLimit {
		return result.Content, fmt.Errorf("sub-agent hit step limit (%d steps) without completing task", result.StepsUsed)
	}
	return result.Content, nil
}

func (a *SubAgent) dispatchDirect(ctx context.Context, req DelegationRequest, dispatcher ToolDispatcher) (string, error) {
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

	toolName := req.Action
	if toolName == "" {
		return "", fmt.Errorf("no action specified in delegation request for step %s", req.StepID)
	}
	if !isAllowed(toolName, allowedTools) {
		return "", fmt.Errorf("tool %q not allowed for agent type %s", toolName, a.Type)
	}
	return dispatcher.Dispatch(ctx, toolName, req.Parameters)
}

func buildUserMessage(req DelegationRequest) string {
	msg := req.Task
	if len(req.Parameters) > 0 {
		msg += "\n\nParameters: " + string(req.Parameters)
	}
	return msg
}

func (a *SubAgent) allowedTools() []string {
	switch a.Type {
	case AgentCode:
		return []string{
			models.ToolReadFile, models.ToolWriteFile, models.ToolEditFile, models.ToolPatchFile, models.ToolApplyDiff,
			models.ToolListFiles, models.ToolCreateDirectory, models.ToolGitStatus, models.ToolGitDiff,
			models.ToolGitCommit, models.ToolGitBranch, models.ToolGitLog, models.ToolRunWorkspaceCmd,
			models.ToolShellExec, models.ToolGotoDefinition, models.ToolFindReferences, models.ToolHoverInfo, models.ToolRenameSymbol,
		}
	case AgentTest:
		return []string{
			models.ToolRunTests, models.ToolExecuteCode, models.ToolReadFile, models.ToolRunWorkspaceCmd, models.ToolShellExec,
		}
	case AgentReview:
		return []string{
			models.ToolReadFile, models.ToolSearchCode, models.ToolListFiles, models.ToolGitDiff, models.ToolGitLog,
			models.ToolGotoDefinition, models.ToolFindReferences, models.ToolHoverInfo,
		}
	default:
		return []string{models.ToolReadFile, models.ToolSearchCode}
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
