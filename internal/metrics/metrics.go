// Package metrics provides Prometheus metrics and OpenTelemetry tracing
// instrumentation for comprehensive observability of the Agent system.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【指标分层】
//
//	HTTP 层     — code_agent_api_request_{total,duration}
//	LLM 层      — code_agent_llm_request_{total,duration}、tokens_used、
//	              circuit_breaker_state
//	Sandbox 层  — code_agent_sandbox_execution_{total,duration}
//	RAG 层      — code_agent_rag_retrieval_duration、reranker_calls
//	MCP 层      — code_agent_mcp_call_{total,duration}
//	Session 层  — code_agent_session_active_count
//	HITL 层     — code_agent_hitl_pending_count
//
// 【label cardinality 控制】
//
//	Prometheus 最大的性能陷阱：label 组合爆炸。规矩：
//	  · 禁止把 user_id / session_id / request_id 放 label；
//	  · method + path（用 route template，不是 raw URL）；
//	  · status 用 HTTP 码字符串；
//	  · provider / model 可控（白名单）；
//	任何一个 label 的取值基数不应超过几十。
//
// 【histogram bucket 选择】
//
//	默认 Prometheus bucket 从 5ms 到 10s。对 LLM 明显不够——一次流式调用
//	可以到 60s 甚至 5min。LLMRequestDuration 用自定义 bucket：
//	1s / 5s / 10s / 30s / 60s / 120s / 300s。粗但够覆盖 P99。
//
// 【metrics 注册的生命周期】
//
//	全部用 promauto 声明成包级变量，import 即注册到 prometheus.DefaultRegisterer。
//	handler 里直接引用。不做显式 Register/Unregister。
//
// ============================================================================
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ─── Prometheus Metrics Registry ─────────────────────────────────────────────

var (
	// ── LLM Metrics ──

	// LLMRequestTotal counts total LLM API requests by provider and status.
	LLMRequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "llm",
		Name:      "request_total",
		Help:      "Total number of LLM API requests",
	}, []string{"provider", "model", "status"})

	// LLMRequestDuration observes LLM API latency in seconds.
	LLMRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "llm",
		Name:      "request_duration_seconds",
		Help:      "LLM API request latency in seconds",
		Buckets:   []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"provider", "model"})

	// LLMTokensUsed tracks token consumption per request.
	LLMTokensUsed = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "llm",
		Name:      "tokens_used_total",
		Help:      "Total tokens consumed by LLM requests",
	}, []string{"provider", "type"}) // type: prompt, completion

	// LLMCircuitBreakerState reports the current circuit breaker state.
	LLMCircuitBreakerState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "code_agent",
		Subsystem: "llm",
		Name:      "circuit_breaker_state",
		Help:      "Circuit breaker state: 0=closed, 1=half-open, 2=open",
	}, []string{"provider"})

	// LLMFallbackTotal counts how many times the fallback provider was used.
	LLMFallbackTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "llm",
		Name:      "fallback_total",
		Help:      "Total number of times the fallback LLM provider was used",
	})

	// ── RAG Metrics ──

	RAGRetrievalDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "rag",
		Name:      "retrieval_duration_seconds",
		Help:      "RAG retrieval latency in seconds",
		Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1},
	})

	RAGChunksReturned = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "rag",
		Name:      "chunks_returned",
		Help:      "Number of chunks returned per RAG query",
		Buckets:   []float64{0, 1, 3, 5, 10, 20},
	})

	// ── Token Pruning Metrics ──

	PrunerTokensSaved = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "pruner",
		Name:      "tokens_saved_total",
		Help:      "Total tokens saved by the pruning engine",
	})

	PrunerChunksPruned = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "pruner",
		Name:      "chunks_pruned_total",
		Help:      "Total code chunks pruned from context",
	})

	// ── Session Metrics ──

	SessionActiveGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "code_agent",
		Subsystem: "session",
		Name:      "active_count",
		Help:      "Number of currently active sessions",
	})

	SessionColdArchiveTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "session",
		Name:      "cold_archive_total",
		Help:      "Total number of messages archived to cold storage",
	})

	SessionContextCompressionTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "session",
		Name:      "context_compression_total",
		Help:      "Total number of context compression events",
	})

	// ── Sandbox Metrics ──

	SandboxExecutionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "sandbox",
		Name:      "execution_total",
		Help:      "Total sandbox executions by language and outcome",
	}, []string{"language", "status"}) // status: success, failed, timeout, oom

	SandboxExecutionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "sandbox",
		Name:      "execution_duration_seconds",
		Help:      "Sandbox execution duration in seconds",
		Buckets:   []float64{0.1, 0.5, 1, 5, 10, 30, 60, 120},
	}, []string{"language"})

	// ── MCP Metrics ──

	MCPCallTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "mcp",
		Name:      "call_total",
		Help:      "Total MCP tool calls by server and tool",
	}, []string{"server", "tool", "status"})

	MCPCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "mcp",
		Name:      "call_duration_seconds",
		Help:      "MCP tool call latency in seconds",
		Buckets:   []float64{0.05, 0.1, 0.5, 1, 5, 10},
	}, []string{"server"})

	// ── HITL Metrics ──

	HITLApprovalTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "hitl",
		Name:      "approval_total",
		Help:      "Total HITL approval decisions",
	}, []string{"decision"}) // approved, rejected, timeout

	HITLPendingGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "code_agent",
		Subsystem: "hitl",
		Name:      "pending_count",
		Help:      "Number of tasks currently awaiting human approval",
	})

	// ── API Metrics ──

	APIRequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "code_agent",
		Subsystem: "api",
		Name:      "request_total",
		Help:      "Total API requests by method, path, and status",
	}, []string{"method", "path", "status"})

	APIRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "code_agent",
		Subsystem: "api",
		Name:      "request_duration_seconds",
		Help:      "API request latency in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method", "path"})

	APIWebSocketConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "code_agent",
		Subsystem: "api",
		Name:      "websocket_connections",
		Help:      "Current number of active WebSocket connections",
	})

	// ── KV Cache Metrics ──

	PromptCacheHitRatio = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "code_agent",
		Subsystem: "prompt",
		Name:      "cache_prefix_hash",
		Help:      "Current prompt prefix hash (changes indicate cache invalidation)",
	}, []string{"hash"})
)
