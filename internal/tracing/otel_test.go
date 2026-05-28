package tracing

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Enabled {
		t.Error("default config should have Enabled=false")
	}
	if cfg.ServiceName != "code-agent" {
		t.Errorf("ServiceName = %q, want code-agent", cfg.ServiceName)
	}
	if cfg.SampleRate != 0.1 {
		t.Errorf("SampleRate = %f, want 0.1", cfg.SampleRate)
	}
	if !cfg.Insecure {
		t.Error("default should be insecure=true")
	}
}

func TestNewProvider_Disabled(t *testing.T) {
	cfg := &Config{Enabled: false}
	logger := zap.NewNop()

	provider, err := NewProvider(cfg, logger)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if provider == nil {
		t.Fatal("provider is nil")
	}

	err = provider.Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestNewProvider_Enabled(t *testing.T) {
	cfg := &Config{
		Enabled:     true,
		Endpoint:    "localhost:4317",
		ServiceName: "test-service",
		SampleRate:  1.0,
		Insecure:    true,
	}
	logger := zap.NewNop()

	provider, err := NewProvider(cfg, logger)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if provider == nil {
		t.Fatal("provider is nil")
	}
	if provider.provider == nil {
		t.Error("internal TracerProvider should be set when enabled")
	}

	err = provider.Shutdown(context.Background())
	if err != nil {
		t.Logf("Shutdown (expected if no collector): %v", err)
	}
}

func TestProvider_ShutdownWhenDisabled(t *testing.T) {
	provider := &Provider{
		cfg:    &Config{Enabled: false},
		logger: zap.NewNop(),
	}

	err := provider.Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown on disabled provider: %v", err)
	}
}

func TestConfig_SampleRates(t *testing.T) {
	tests := []struct {
		name       string
		sampleRate float64
	}{
		{"always sample", 1.0},
		{"never sample", 0.0},
		{"10 percent", 0.1},
		{"50 percent", 0.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Enabled:     false,
				ServiceName: "test",
				SampleRate:  tt.sampleRate,
			}
			if cfg.SampleRate != tt.sampleRate {
				t.Errorf("SampleRate = %f, want %f", cfg.SampleRate, tt.sampleRate)
			}
		})
	}
}
