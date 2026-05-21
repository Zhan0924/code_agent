package config

import (
	"strings"
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		Server: ServerConfig{
			HTTPAddr:        ":8080",
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    60 * time.Second,
			ShutdownTimeout: 15 * time.Second,
		},
		LLM: LLMConfig{
			Primary: LLMProviderConfig{
				Provider:  "openai",
				APIKey:    "sk-test",
				Model:     "gpt-4o",
				MaxTokens: 4096,
				Timeout:   60 * time.Second,
			},
			CircuitBreaker: CircuitBreakerConfig{
				MaxFailures: 5,
				Timeout:     30 * time.Second,
			},
		},
		Redis: RedisConfig{
			Addr:     "localhost:6379",
			PoolSize: 20,
		},
		Session: SessionConfig{
			MaxHistoryTokens:       4000,
			SummaryThresholdTokens: 3500,
			TTL:                    24 * time.Hour,
		},
		RAG: RAGConfig{
			ChunkMaxTokens: 512,
			TopK:           10,
		},
		Sandbox: SandboxConfig{
			Timeout: 120 * time.Second,
		},
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config should not produce errors, got: %v", err)
	}
}

func TestValidate_MissingHTTPAddr(t *testing.T) {
	cfg := validConfig()
	cfg.Server.HTTPAddr = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty http_addr")
	}
	if !strings.Contains(err.Error(), "http_addr") {
		t.Errorf("error should mention http_addr: %v", err)
	}
}

func TestValidate_MissingLLMModel(t *testing.T) {
	cfg := validConfig()
	cfg.LLM.Primary.Model = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty model")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("error should mention model: %v", err)
	}
}

func TestValidate_NegativeMaxTokens(t *testing.T) {
	cfg := validConfig()
	cfg.LLM.Primary.MaxTokens = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative max_tokens")
	}
}

func TestValidate_SummaryExceedsMax(t *testing.T) {
	cfg := validConfig()
	cfg.Session.SummaryThresholdTokens = 5000
	cfg.Session.MaxHistoryTokens = 4000
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error when summary_threshold > max_history")
	}
	if !strings.Contains(err.Error(), "summary_threshold_tokens") {
		t.Errorf("error should mention summary_threshold: %v", err)
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := &Config{}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected multiple validation errors for empty config")
	}
	// Should contain multiple errors
	errMsg := err.Error()
	if count := strings.Count(errMsg, "\n"); count < 3 {
		t.Errorf("expected many errors for empty config, got %d line breaks: %s", count, errMsg)
	}
}
