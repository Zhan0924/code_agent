// Cost-attribution metrics for LLM usage.
//
// These metrics complement the lower-cardinality LLMTokensUsed counter by
// giving operators a way to attribute spend to sessions, users and tasks —
// which is a table-stake requirement for enterprise (ToB) deployments where
// "how much did this session cost me" is a first-class question.
//
// NOTE ON CARDINALITY
// ────────────────────────────────────────────────────────────────────────────
// user_id and session_id are intentionally high-cardinality labels.
// In production, scrape these metrics with short retention and/or drop the
// high-cardinality labels at the remote-write layer (e.g. Prometheus
// relabel_configs) if you don't need per-session breakdown. For durable
// spend reporting, use the `agent_cost_ledger` SQL table instead — see
// internal/store.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// LLMCostUSD accumulates estimated USD cost per LLM call.
	LLMCostUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "llm",
		Name:      "cost_usd_total",
		Help:      "Estimated LLM spend in USD, attributed to session/user/task",
	}, []string{"model", "tier", "session_id", "user_id", "task_id"})

	// ToolExecutionDuration observes the end-to-end latency of any tool the
	// orchestrator invokes — builtin, MCP, skill, or planner step.
	ToolExecutionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "tool",
		Name:      "execution_duration_seconds",
		Help:      "Tool execution latency in seconds",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 14), // 10ms … ~80s
	}, []string{"tool", "source", "status"})

	// ToolExecutionTotal counts individual tool invocations for outcome tracking.
	ToolExecutionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "tool",
		Name:      "execution_total",
		Help:      "Total tool executions, partitioned by source and outcome",
	}, []string{"tool", "source", "status"})

	// PlannerStepsTotal counts plan steps by terminal status. Useful to spot
	// chronic failure modes (e.g. "code_edit always fails on a given repo").
	PlannerStepsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "planner",
		Name:      "steps_total",
		Help:      "Plan steps executed, partitioned by terminal status",
	}, []string{"action", "status"})

	// PlannerRevisionTotal tracks how often we fall into the plan-revise loop.
	PlannerRevisionTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "planner",
		Name:      "revision_total",
		Help:      "Total plan revisions triggered by step failures",
	})

	// PlannerPlansCreated counts how many plans (not steps) were produced.
	PlannerPlansCreated = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "planner",
		Name:      "plans_created_total",
		Help:      "Total plans created by the Planner",
	})
)

// PricePerModel defines USD per 1K tokens. Extend as new models are onboarded.
// For a full-fidelity system, load this from a YAML at startup instead of hard-coding.
var PricePerModel = map[string]struct {
	InputPer1K  float64
	OutputPer1K float64
}{
	// GPT-4 family (illustrative; adjust per actual contract prices)
	"gpt-4o":        {InputPer1K: 0.005, OutputPer1K: 0.015},
	"gpt-4o-mini":   {InputPer1K: 0.00015, OutputPer1K: 0.0006},
	"gpt-4-turbo":   {InputPer1K: 0.01, OutputPer1K: 0.03},
	"gpt-3.5-turbo": {InputPer1K: 0.0005, OutputPer1K: 0.0015},
	// Claude family
	"claude-3-5-sonnet": {InputPer1K: 0.003, OutputPer1K: 0.015},
	"claude-3-haiku":    {InputPer1K: 0.00025, OutputPer1K: 0.00125},
	// DeepSeek (local/self-hosted placeholders)
	"deepseek-coder": {InputPer1K: 0.00014, OutputPer1K: 0.00028},
	"deepseek-chat":  {InputPer1K: 0.00014, OutputPer1K: 0.00028},
	"qwen2.5-coder":  {InputPer1K: 0.00010, OutputPer1K: 0.00020},
	"llama-3.1-70b":  {InputPer1K: 0.0004, OutputPer1K: 0.0004},
}

// EstimateCostUSD returns the USD cost of a single LLM call for the given
// model and token counts. Unknown models yield 0 (they still increment the
// token counters, but don't pollute the cost gauge).
func EstimateCostUSD(model string, inputTokens, outputTokens int) float64 {
	price, ok := PricePerModel[model]
	if !ok {
		return 0
	}
	return (float64(inputTokens)/1000.0)*price.InputPer1K +
		(float64(outputTokens)/1000.0)*price.OutputPer1K
}

// RecordLLMCost increments the cost counter with all attribution labels.
// Call this from the LLM client once a response has been received.
//
// Any label that isn't known at the call site should be passed as "unknown"
// to avoid empty-string label values (which some Prometheus setups reject).
func RecordLLMCost(model, tier, sessionID, userID, taskID string, inputTokens, outputTokens int) {
	cost := EstimateCostUSD(model, inputTokens, outputTokens)
	if cost <= 0 {
		return
	}
	LLMCostUSD.WithLabelValues(
		nonEmpty(model),
		nonEmpty(tier),
		nonEmpty(sessionID),
		nonEmpty(userID),
		nonEmpty(taskID),
	).Add(cost)
}

func nonEmpty(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
