package metrics

import (
	"testing"
)

func TestEstimateCostUSD_KnownModel(t *testing.T) {
	cost := EstimateCostUSD("gpt-4o", 1000, 500)
	// 1000/1000 * 0.005 + 500/1000 * 0.015 = 0.005 + 0.0075 = 0.0125
	expected := 0.0125
	if cost != expected {
		t.Errorf("EstimateCostUSD(gpt-4o, 1000, 500) = %f, want %f", cost, expected)
	}
}

func TestEstimateCostUSD_UnknownModel(t *testing.T) {
	cost := EstimateCostUSD("unknown-model", 1000, 500)
	if cost != 0 {
		t.Errorf("EstimateCostUSD(unknown) = %f, want 0", cost)
	}
}

func TestEstimateCostUSD_ZeroTokens(t *testing.T) {
	cost := EstimateCostUSD("gpt-4o", 0, 0)
	if cost != 0 {
		t.Errorf("EstimateCostUSD(0,0) = %f, want 0", cost)
	}
}

func TestNonEmpty(t *testing.T) {
	if got := nonEmpty(""); got != "unknown" {
		t.Errorf("nonEmpty(\"\") = %q, want \"unknown\"", got)
	}
	if got := nonEmpty("hello"); got != "hello" {
		t.Errorf("nonEmpty(\"hello\") = %q, want \"hello\"", got)
	}
}

func TestRecordLLMCost_DoesNotPanic(t *testing.T) {
	// Verify recording cost doesn't panic with valid inputs
	RecordLLMCost("gpt-4o", "standard", "sess-1", "user-1", "task-1", 100, 50)
	RecordLLMCost("unknown-model", "standard", "sess-1", "user-1", "task-1", 100, 50)
	RecordLLMCost("gpt-4o", "", "", "", "", 100, 50)
}

func TestMetricsRegistered(t *testing.T) {
	// Verify all metrics are non-nil (promauto registers on init)
	if LLMRequestTotal == nil {
		t.Error("LLMRequestTotal is nil")
	}
	if LLMRequestDuration == nil {
		t.Error("LLMRequestDuration is nil")
	}
	if LLMTokensUsed == nil {
		t.Error("LLMTokensUsed is nil")
	}
	if RAGRetrievalDuration == nil {
		t.Error("RAGRetrievalDuration is nil")
	}
	if SandboxExecutionTotal == nil {
		t.Error("SandboxExecutionTotal is nil")
	}
	if MCPCallTotal == nil {
		t.Error("MCPCallTotal is nil")
	}
	if ToolExecutionDuration == nil {
		t.Error("ToolExecutionDuration is nil")
	}
	if ToolExecutionTotal == nil {
		t.Error("ToolExecutionTotal is nil")
	}
}

func TestMetricsIncrement(t *testing.T) {
	// Verify counters can be incremented without panic
	LLMRequestTotal.WithLabelValues("openai", "gpt-4o", "success").Inc()
	LLMTokensUsed.WithLabelValues("openai", "prompt").Add(100)
	SandboxExecutionTotal.WithLabelValues("go", "success").Inc()
	MCPCallTotal.WithLabelValues("server1", "tool1", "success").Inc()
	ToolExecutionTotal.WithLabelValues("read_file", "builtin", "success").Inc()
	SessionActiveGauge.Set(5)
	HITLPendingGauge.Set(2)
}

func TestHistogramObserve(t *testing.T) {
	LLMRequestDuration.WithLabelValues("openai", "gpt-4o").Observe(1.5)
	RAGRetrievalDuration.Observe(0.1)
	RAGChunksReturned.Observe(5)
	SandboxExecutionDuration.WithLabelValues("python").Observe(2.0)
	MCPCallDuration.WithLabelValues("server1").Observe(0.5)
	ToolExecutionDuration.WithLabelValues("run_command", "builtin", "success").Observe(3.0)
	APIRequestDuration.WithLabelValues("GET", "/api/v1/chat").Observe(0.2)
}

func TestPricePerModel_Coverage(t *testing.T) {
	models := []string{"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-3.5-turbo",
		"claude-3-5-sonnet", "claude-3-haiku", "deepseek-coder", "deepseek-chat",
		"qwen2.5-coder", "llama-3.1-70b"}

	for _, m := range models {
		price, ok := PricePerModel[m]
		if !ok {
			t.Errorf("model %q not in PricePerModel", m)
			continue
		}
		if price.InputPer1K <= 0 {
			t.Errorf("model %q has non-positive InputPer1K", m)
		}
		if price.OutputPer1K <= 0 {
			t.Errorf("model %q has non-positive OutputPer1K", m)
		}
	}
}
