// Package audit provides structured audit logging for sensitive operations.
// [OPT-15] All security-sensitive operations (HITL approvals, sandbox executions,
// MCP tool calls, deployment operations) are logged with structured fields
// for compliance, debugging, and forensic analysis.
package audit

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// EventType classifies the kind of auditable action.
type EventType string

const (
	EventApprovalRequested EventType = "approval_requested"
	EventApprovalGranted   EventType = "approval_granted"
	EventApprovalDenied    EventType = "approval_denied"
	EventApprovalTimeout   EventType = "approval_timeout"
	EventSandboxExecution  EventType = "sandbox_execution"
	EventMCPToolCall       EventType = "mcp_tool_call"
	EventSensitiveBlocked  EventType = "sensitive_blocked"
	EventSessionCreated    EventType = "session_created"
	EventSessionDeleted    EventType = "session_deleted"
	EventIndexingStarted   EventType = "indexing_started"
)

// Event represents a single audit log entry.
type Event struct {
	Timestamp time.Time         `json:"timestamp"`
	Type      EventType         `json:"type"`
	SessionID string            `json:"session_id,omitempty"`
	TaskID    string            `json:"task_id,omitempty"`
	UserID    string            `json:"user_id,omitempty"`
	Action    string            `json:"action"`
	Details   map[string]string `json:"details,omitempty"`
	IP        string            `json:"ip,omitempty"`
	Success   bool              `json:"success"`
	Error     string            `json:"error,omitempty"`
}

// Logger is a structured audit logger that writes to a dedicated zap logger.
// In production, this logger's output can be directed to a separate file,
// Elasticsearch, or a SIEM system via zap's output configuration.
type Logger struct {
	logger *zap.Logger
}

// NewLogger creates a new audit logger.
func NewLogger(baseLogger *zap.Logger) *Logger {
	return &Logger{
		logger: baseLogger.Named("audit").With(zap.String("log_type", "audit")),
	}
}

// Log records an audit event with structured fields.
func (l *Logger) Log(_ context.Context, event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	fields := []zap.Field{
		zap.Time("event_time", event.Timestamp),
		zap.String("event_type", string(event.Type)),
		zap.String("action", event.Action),
		zap.Bool("success", event.Success),
	}

	if event.SessionID != "" {
		fields = append(fields, zap.String("session_id", event.SessionID))
	}
	if event.TaskID != "" {
		fields = append(fields, zap.String("task_id", event.TaskID))
	}
	if event.UserID != "" {
		fields = append(fields, zap.String("user_id", event.UserID))
	}
	if event.IP != "" {
		fields = append(fields, zap.String("client_ip", event.IP))
	}
	if event.Error != "" {
		fields = append(fields, zap.String("error", event.Error))
	}
	if len(event.Details) > 0 {
		for k, v := range event.Details {
			fields = append(fields, zap.String("detail_"+k, v))
		}
	}

	l.logger.Info("audit_event", fields...)
}

// LogApproval is a convenience method for HITL approval events.
func (l *Logger) LogApproval(ctx context.Context, eventType EventType, taskID, sessionID, action string, success bool) {
	l.Log(ctx, Event{
		Type:      eventType,
		TaskID:    taskID,
		SessionID: sessionID,
		Action:    action,
		Success:   success,
	})
}

// LogSandboxExec logs a sandbox code execution event.
func (l *Logger) LogSandboxExec(ctx context.Context, sessionID, language string, exitCode int, duration time.Duration) {
	l.Log(ctx, Event{
		Type:      EventSandboxExecution,
		SessionID: sessionID,
		Action:    "execute_code",
		Details: map[string]string{
			"language":  language,
			"exit_code": string(rune(exitCode + '0')),
			"duration":  duration.String(),
		},
		Success: exitCode == 0,
	})
}

// LogMCPCall logs an MCP tool invocation event.
func (l *Logger) LogMCPCall(ctx context.Context, serverName, toolName string, success bool, errMsg string) {
	l.Log(ctx, Event{
		Type:   EventMCPToolCall,
		Action: toolName,
		Details: map[string]string{
			"server": serverName,
		},
		Success: success,
		Error:   errMsg,
	})
}
