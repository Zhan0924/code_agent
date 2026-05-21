// Package models defines the core domain types used throughout the Code Agent system.
// These types represent tasks, sessions, tools, and inter-component messages.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么要一个 models 包】
//
//	多个子包需要互相传递同一类型（Session 从 session 包构造、handler 写回
//	响应、orchestrator 追加消息）。如果 types 定义在 session 包里，handler
//	要 import session，orchestrator 也要，渐渐所有人都 import session →
//	循环依赖。把所有跨子包共享的 DTO 都放在 models，每个子包都能零成本用。
//
// 【稳定 API 的准则】
//
//	这个包里的类型一旦定义就基本是"前端约定"——JSON schema 直接决定 UI
//	怎么渲染。所以：
//	  · 加字段：OK（带 omitempty）
//	  · 删字段：破坏性，必须小心
//	  · 改字段类型：禁止——除非灰度切换新字段
//	  · 改 JSON tag：禁止
//
// 【Message / ToolCall / ToolResult 三件套】
//
//	LLM 对话的基石。严格按 OpenAI schema 映射：
//	  · Message = {role: "user"/"assistant"/"system"/"tool", content, tool_calls, tool_call_id}
//	  · ToolCall = {id, type, function: {name, arguments}}
//	  · ToolResult = {tool_call_id, content, is_error}
//	这样前端、orchestrator、provider 三层用同一套，不需要翻译。
//
// 【TaskIntent 的 enum】
//
//	· IntentCodeQuery    — 代码问答
//	· IntentCodeExecute  — 执行代码
//	· IntentDiagnose     — 诊断 / 查日志
//	· IntentDeploy       — **必然触发 HITL**
//	· IntentMCPCall      — 走 MCP
//	· IntentConversation — 闲聊
//	新增 intent 必须同步更新：orchestrator.parseIntent 的 switch + 对应
//	system prompt + HITL 判定逻辑。
//
// ============================================================================
package models

import (
	"encoding/json"
	"time"
)

// ─── Task & Workflow Models ─────────────────────────────────────────────────

// TaskState represents the finite state of a task in the orchestrator FSM.
type TaskState string

const (
	TaskStatePending   TaskState = "pending"
	TaskStatePlanning  TaskState = "planning"
	TaskStateExecuting TaskState = "executing"
	TaskStateSuspended TaskState = "suspended" // HITL: waiting for human approval
	TaskStateCompleted TaskState = "completed"
	TaskStateFailed    TaskState = "failed"
	TaskStateCancelled TaskState = "cancelled"
)

// TaskIntent represents the parsed intent type from user input.
type TaskIntent string

const (
	IntentCodeQuery    TaskIntent = "code_query"   // Ask about code / docs
	IntentCodeExecute  TaskIntent = "code_execute" // Execute code in sandbox
	IntentDiagnose     TaskIntent = "diagnose"     // Run diagnostic scripts
	IntentDeploy       TaskIntent = "deploy"       // Deployment operations
	IntentMCPCall      TaskIntent = "mcp_call"     // External tool via MCP
	IntentConversation TaskIntent = "conversation" // General conversation
)

// Task represents a single unit of work managed by the orchestrator.
type Task struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"session_id"`
	UserInput   string          `json:"user_input"`
	Intent      TaskIntent      `json:"intent"`
	State       TaskState       `json:"state"`
	Plan        *ExecutionPlan  `json:"plan,omitempty"`
	Result      *TaskResult     `json:"result,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
}

// ExecutionPlan describes the sequence of steps the agent plans to execute.
type ExecutionPlan struct {
	Steps            []PlanStep `json:"steps"`
	Reasoning        string     `json:"reasoning"`
	RequiresApproval bool       `json:"requires_approval"`
}

// PlanStep is a single step within an execution plan.
type PlanStep struct {
	ID          string          `json:"id"`
	Type        StepType        `json:"type"`
	Description string          `json:"description"`
	Tool        string          `json:"tool,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	DependsOn   []string        `json:"depends_on,omitempty"`
	Status      TaskState       `json:"status"`
	Output      string          `json:"output,omitempty"`
}

// StepType classifies the kind of plan step.
type StepType string

const (
	StepTypeLLMCall  StepType = "llm_call"
	StepTypeRAGQuery StepType = "rag_query"
	StepTypeSandbox  StepType = "sandbox_exec"
	StepTypeMCPTool  StepType = "mcp_tool"
	StepTypeHITL     StepType = "hitl_approval"
)

// TaskResult holds the final output of a completed task.
type TaskResult struct {
	Success bool   `json:"success"`
	Output  string `json:"output"`
	Error   string `json:"error,omitempty"`
}

// ─── Session & Message Models ────────────────────────────────────────────────

// Role defines the role in a conversation message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// Message represents a single message in a conversation session.
type Message struct {
	ID         string          `json:"id"`
	Role       Role            `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Timestamp  time.Time       `json:"timestamp"`
	TokenCount int             `json:"token_count"`
}

// Session represents a conversation session with sliding window context.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Messages  []Message `json:"messages"`
	Summary   string    `json:"summary,omitempty"` // compressed history summary
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ─── Tool & MCP Models ───────────────────────────────────────────────────────

// ToolCall represents an LLM function/tool call request.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"arguments"`
}

// ToolDefinition describes a tool available to the LLM via function calling.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
	Source      string          `json:"source"`     // "builtin", "mcp:<server_name>"
}

// ToolResult represents the output of a tool execution.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
}

// ─── Sandbox Models ──────────────────────────────────────────────────────────

// SandboxRequest specifies a code execution request for the Docker sandbox.
type SandboxRequest struct {
	Language     string            `json:"language"` // python, go, bash, node
	Code         string            `json:"code"`
	Files        map[string]string `json:"files,omitempty"` // additional files to mount
	Env          map[string]string `json:"env,omitempty"`   // environment variables
	Timeout      time.Duration     `json:"timeout,omitempty"`
	StreamOutput bool              `json:"stream_output"`
}

// SandboxResult holds the output of a sandbox execution.
type SandboxResult struct {
	ExitCode int           `json:"exit_code"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	Duration time.Duration `json:"duration"`
	Killed   bool          `json:"killed"` // true if timeout/OOM killed
}

// ─── RAG Models ──────────────────────────────────────────────────────────────

// CodeChunk represents a parsed and indexed code segment.
type CodeChunk struct {
	ID           string            `json:"id"`
	FilePath     string            `json:"file_path"`
	Language     string            `json:"language"`
	SymbolName   string            `json:"symbol_name,omitempty"` // function/class name
	SymbolType   string            `json:"symbol_type,omitempty"` // function, class, method
	Content      string            `json:"content"`
	StartLine    int               `json:"start_line"`
	EndLine      int               `json:"end_line"`
	ScopeDepth   int               `json:"scope_depth,omitempty"`  // AST nesting depth for token pruning
	Dependencies []string          `json:"dependencies,omitempty"` // called symbols
	Metadata     map[string]string `json:"metadata,omitempty"`
	Embedding    []float32         `json:"embedding,omitempty"`
}

// RetrievalResult represents a single result from the RAG retrieval pipeline.
type RetrievalResult struct {
	Chunk  CodeChunk `json:"chunk"`
	Score  float64   `json:"score"`
	Source string    `json:"source"` // "dense", "sparse", "reranked"
}

// ─── HITL (Human-in-the-Loop) Models ────────────────────────────────────────

// ApprovalRequest is sent to the frontend when a task requires human approval.
type ApprovalRequest struct {
	TaskID      string    `json:"task_id"`
	SessionID   string    `json:"session_id"`
	Action      string    `json:"action"`     // description of what needs approval
	RiskLevel   string    `json:"risk_level"` // "low", "medium", "high", "critical"
	Details     string    `json:"details"`    // full command/query text
	RequestedAt time.Time `json:"requested_at"`
}

// ApprovalResponse is the user's response to an ApprovalRequest.
type ApprovalResponse struct {
	TaskID   string          `json:"task_id"`
	Approved bool            `json:"approved"`
	Comment  string          `json:"comment,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"` // optional additional parameters
}

// ─── API Request/Response Models ──────────────────────────────────────────────

// ChatRequest is the API request body for the chat endpoint.
type ChatRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Message   string `json:"message" binding:"required"`
	Stream    bool   `json:"stream"`
}

// ChatResponse is the API response for the chat endpoint.
type ChatResponse struct {
	SessionID string           `json:"session_id"`
	TaskID    string           `json:"task_id"`
	Message   string           `json:"message,omitempty"`
	State     TaskState        `json:"state"`
	Approval  *ApprovalRequest `json:"approval,omitempty"`
}

// StreamEvent represents a single SSE event pushed to the client.
type StreamEvent struct {
	Type   string          `json:"type"` // "session", "step_start", "thinking", "tool_call", "tool_result", "message", "approval", "done", "error"
	Data   json.RawMessage `json:"data"`
	TaskID string          `json:"task_id,omitempty"`
}

// ReactStreamEvent is a structured event emitted during the ReAct loop.
type ReactStreamEvent struct {
	Type       string      `json:"type"`                   // "step_start", "thinking", "tool_call", "tool_result", "rag_context", "message", "error", "done"
	Step       int         `json:"step,omitempty"`         // Current ReAct step number (1-based)
	MaxSteps   int         `json:"max_steps,omitempty"`    // Maximum steps allowed
	Content    string      `json:"content,omitempty"`      // Text content (thinking, message, error)
	ToolName   string      `json:"tool_name,omitempty"`    // Tool being called
	ToolArgs   string      `json:"tool_args,omitempty"`    // Tool arguments (JSON string)
	ToolCallID string      `json:"tool_call_id,omitempty"` // Tool call ID for correlation
	IsError    bool        `json:"is_error,omitempty"`     // Whether tool result is an error
	Intent     string      `json:"intent,omitempty"`       // Detected intent (for step_start)
	TaskID     string      `json:"task_id,omitempty"`      // Task ID
	Metadata   interface{} `json:"metadata,omitempty"`     // Additional metadata
}
