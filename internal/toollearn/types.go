package toollearn

import "time"

// Feedback records the outcome of a single tool invocation.
type Feedback struct {
	ID        int64     `json:"id" db:"id"`
	ToolName  string    `json:"tool_name" db:"tool_name"`
	ArgsHash  string    `json:"args_hash" db:"args_hash"`
	Success   bool      `json:"success" db:"success"`
	Duration  int       `json:"duration_ms" db:"duration_ms"`
	ErrorMsg  string    `json:"error_msg,omitempty" db:"error_msg"`
	SessionID string    `json:"session_id" db:"session_id"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// ToolPattern represents a learned pattern about tool usage.
type ToolPattern struct {
	ToolName       string  `json:"tool_name"`
	FailureRate    float64 `json:"failure_rate"`
	AvgDuration    int     `json:"avg_duration_ms"`
	CommonErrors   []string `json:"common_errors,omitempty"`
	SuccessHints   []string `json:"success_hints,omitempty"`
	SampleSize     int     `json:"sample_size"`
}

// Advice is a recommendation provided before tool dispatch.
type Advice struct {
	ToolName string `json:"tool_name"`
	Warning  string `json:"warning,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

// Store abstracts persistence for tool feedback data.
type Store interface {
	RecordFeedback(fb *Feedback) error
	GetPatterns(toolName string, limit int) ([]ToolPattern, error)
	GetRecentFailures(toolName string, window time.Duration) ([]Feedback, error)
}
