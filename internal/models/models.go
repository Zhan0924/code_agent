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
	ID           string          `json:"id"`
	SessionID    string          `json:"session_id"`
	UserInput    string          `json:"user_input"`
	Intent       TaskIntent      `json:"intent"`
	State        TaskState       `json:"state"`
	Plan         *ExecutionPlan  `json:"plan,omitempty"`
	Result       *TaskResult     `json:"result,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	OutputFormat *ResponseFormat `json:"output_format,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	CompletedAt  *time.Time      `json:"completed_at,omitempty"`

	// VerificationRetried is a runtime-only sentinel that gates the
	// stream-path "fix once" loop after verifyOutput rejects an answer.
	// Set to true the moment a retry is triggered so a chronically-failing
	// LLM cannot ping-pong the agent indefinitely (one retry max per task).
	VerificationRetried bool `json:"-"`
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

// CacheControl specifies prompt caching behavior for a message.
// When set, the LLM provider marks this message as a cache breakpoint,
// enabling significant cost savings (40-90%) and latency reduction (up to 10x)
// for Anthropic-compatible providers.
type CacheControl struct {
	Type string `json:"type"` // "ephemeral" — cached for the duration of the request
}

// Message represents a single message in a conversation session.
type Message struct {
	ID           string          `json:"id"`
	Role         Role            `json:"role"`
	Content      string          `json:"content"`
	ToolCalls    []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID   string          `json:"tool_call_id,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	Timestamp    time.Time       `json:"timestamp"`
	TokenCount       int             `json:"token_count"`
	CitedMemoryIDs   []string        `json:"cited_memory_ids,omitempty"`
	CacheControl     *CacheControl   `json:"cache_control,omitempty"`
	Pinned       bool            `json:"pinned,omitempty"`
}

// Session represents a conversation session with sliding window context.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	ProjectID string    `json:"project_id,omitempty"`
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
//
// Behavior metadata fields (IsFileWrite, IsIdempotentRead, TriggersAutoTest,
// InvalidatesCache) centralize what used to be hardcoded tool-name lists
// scattered across 9 sites (react_core, supervisor, captureForTransaction,
// speculative_cache, dynamic_tool_handlers, mcp_skill_handlers, ...).
// The source of truth for builtin tools is `fileToolDefinitions()` /
// `gitToolDefinitions()` — set the bits there once.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
	Source      string          `json:"source"`     // "builtin", "mcp:<server_name>"
	RiskLevel   int             `json:"risk_level"` // 0=safe, 1=moderate, 2=high (requires approval)

	// IsFileWrite marks a tool that mutates files in the workspace —
	// used by transaction capture, auto-test triggering, and conflict
	// detection in the multi-agent supervisor.
	IsFileWrite bool `json:"is_file_write,omitempty"`

	// IsIdempotentRead marks a side-effect-free read tool whose result
	// can be cached by the speculative-tool cache.
	IsIdempotentRead bool `json:"is_idempotent_read,omitempty"`

	// TriggersAutoTest marks a tool whose successful execution should
	// trigger the auto-test runner (typically equals IsFileWrite, but
	// kept separate so the policy can diverge).
	TriggersAutoTest bool `json:"triggers_auto_test,omitempty"`

	// InvalidatesCache marks a tool that should clear the speculative
	// read cache after a successful run (write-after-read invalidation).
	InvalidatesCache bool `json:"invalidates_cache,omitempty"`
}

// ToolResult represents the output of a tool execution.
//
// Metadata carries optional structured payload (e.g. unified diff for
// edit_file/apply_diff) that the SSE pipeline relays into
// ReactStreamEvent.Metadata so the UI can render it as a richer artifact
// alongside the human-readable Content. Using json.RawMessage avoids the
// type-erasure that map[string]any would incur on the relay.
type ToolResult struct {
	ToolCallID string          `json:"tool_call_id"`
	Content    string          `json:"content"`
	IsError    bool            `json:"is_error"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
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

// ─── Structured Output Models ────────────────────────────────────────────────

// ResponseFormatType defines the output format type for LLM responses.
type ResponseFormatType string

const (
	ResponseFormatText       ResponseFormatType = "text"
	ResponseFormatJSONObject ResponseFormatType = "json_object"
	ResponseFormatJSONSchema ResponseFormatType = "json_schema"
)

// ResponseFormat defines LLM output format constraints (maps to OpenAI response_format).
type ResponseFormat struct {
	Type       ResponseFormatType `json:"type"`
	JSONSchema *JSONSchemaFormat  `json:"json_schema,omitempty"`
}

// JSONSchemaFormat defines the JSON Schema constraint for structured output.
type JSONSchemaFormat struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Strict      bool            `json:"strict"`
}

// ─── API Request/Response Models ──────────────────────────────────────────────

// ChatRequest is the API request body for the chat endpoint.
type ChatRequest struct {
	SessionID    string          `json:"session_id,omitempty"`
	Message      string          `json:"message" binding:"required"`
	Stream       bool            `json:"stream"`
	OutputFormat *ResponseFormat `json:"output_format,omitempty"`
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
	Type       string      `json:"type"`                   // "step_start", "thinking", "tool_call", "tool_result", "rag_context", "message", "verification_warning", "error", "done"
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
