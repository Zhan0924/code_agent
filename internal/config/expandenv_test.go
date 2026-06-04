package config

import (
	"reflect"
	"testing"
)

// nestedFixture exercises every kind walkExpandEnv handles: a top-level
// string, a string with the opt-out tag, a nested struct, a string slice, a
// slice of structs, and a string-keyed string map. Used by the test below to
// confirm both the expansion behaviour and the env_expand:"false" guard.
type nestedFixture struct {
	APIKey      string
	Literal     string `env_expand:"false"`
	Inner       innerFixture
	StringSlice []string
	Servers     []innerFixture
	Env         map[string]string
}

type innerFixture struct {
	URL  string
	Name string
}

// TestWalkExpandEnv_ExpandsAllStringFields pins the contract for PR 7: every
// string field in the tree gets ${VAR} expanded unless explicitly opted out.
// Before the reflection refactor, only 18 hand-listed fields were expanded
// — any new field that contained ${...} silently passed through unexpanded.
func TestWalkExpandEnv_ExpandsAllStringFields(t *testing.T) {
	t.Setenv("EXPAND_TEST_VAR", "expanded-value")

	fx := &nestedFixture{
		APIKey:  "${EXPAND_TEST_VAR}",
		Literal: "${EXPAND_TEST_VAR}",
		Inner: innerFixture{
			URL:  "https://${EXPAND_TEST_VAR}.example.com",
			Name: "static-name",
		},
		StringSlice: []string{"${EXPAND_TEST_VAR}", "static"},
		Servers: []innerFixture{
			{URL: "${EXPAND_TEST_VAR}-1", Name: "srv1"},
			{URL: "${EXPAND_TEST_VAR}-2", Name: "srv2"},
		},
		Env: map[string]string{
			"KEY": "${EXPAND_TEST_VAR}",
			"LIT": "no-var-here",
		},
	}

	walkExpandEnv(reflect.ValueOf(fx).Elem())

	if fx.APIKey != "expanded-value" {
		t.Errorf("APIKey not expanded: %q", fx.APIKey)
	}
	if fx.Literal != "${EXPAND_TEST_VAR}" {
		t.Errorf("Literal must NOT be expanded (env_expand:\"false\"): %q", fx.Literal)
	}
	if fx.Inner.URL != "https://expanded-value.example.com" {
		t.Errorf("Inner.URL not expanded: %q", fx.Inner.URL)
	}
	if fx.Inner.Name != "static-name" {
		t.Errorf("Inner.Name (no $) should pass through: %q", fx.Inner.Name)
	}
	if fx.StringSlice[0] != "expanded-value" || fx.StringSlice[1] != "static" {
		t.Errorf("StringSlice expansion wrong: %v", fx.StringSlice)
	}
	if fx.Servers[0].URL != "expanded-value-1" || fx.Servers[1].URL != "expanded-value-2" {
		t.Errorf("Servers slice expansion wrong: %v", fx.Servers)
	}
	if fx.Env["KEY"] != "expanded-value" {
		t.Errorf("Env map expansion wrong: %v", fx.Env)
	}
	if fx.Env["LIT"] != "no-var-here" {
		t.Errorf("Env literal value mutated: %v", fx.Env)
	}
}

// TestWalkExpandEnv_RealConfigShape applies the walker to a populated Config
// instance and verifies the well-known fields previously expanded by hand are
// still expanded. This is the regression guard for the 18-field manual list
// migration: if reflection misses a tree branch, this test fires.
func TestWalkExpandEnv_RealConfigShape(t *testing.T) {
	t.Setenv("EXPAND_TEST_CFG", "filled")

	cfg := &Config{}
	cfg.LLM.Primary.APIKey = "${EXPAND_TEST_CFG}"
	cfg.LLM.Primary.BaseURL = "https://${EXPAND_TEST_CFG}/v1"
	cfg.LLM.Fallback.APIKey = "${EXPAND_TEST_CFG}"
	cfg.LLM.Fallback.BaseURL = "${EXPAND_TEST_CFG}"
	cfg.RAG.EmbeddingBaseURL = "${EXPAND_TEST_CFG}"
	cfg.RAG.EmbeddingAPIKey = "${EXPAND_TEST_CFG}"
	cfg.RAG.RerankBaseURL = "${EXPAND_TEST_CFG}"
	cfg.RAG.RerankAPIKey = "${EXPAND_TEST_CFG}"
	cfg.Redis.Addr = "${EXPAND_TEST_CFG}"
	cfg.Redis.Password = "${EXPAND_TEST_CFG}"
	cfg.Postgres.DSN = "${EXPAND_TEST_CFG}"
	cfg.Qdrant.Addr = "${EXPAND_TEST_CFG}"
	cfg.Temporal.Host = "${EXPAND_TEST_CFG}"
	cfg.Auth.JWTSecret = "${EXPAND_TEST_CFG}"
	cfg.Tracing.Endpoint = "${EXPAND_TEST_CFG}"
	cfg.MCP.Servers = []MCPServerConfig{
		{URL: "${EXPAND_TEST_CFG}", Command: "${EXPAND_TEST_CFG}", Args: []string{"${EXPAND_TEST_CFG}"}, Env: map[string]string{"K": "${EXPAND_TEST_CFG}"}},
	}

	walkExpandEnv(reflect.ValueOf(cfg).Elem())

	checks := map[string]string{
		"LLM.Primary.APIKey":   cfg.LLM.Primary.APIKey,
		"LLM.Primary.BaseURL":  cfg.LLM.Primary.BaseURL,
		"LLM.Fallback.APIKey":  cfg.LLM.Fallback.APIKey,
		"LLM.Fallback.BaseURL": cfg.LLM.Fallback.BaseURL,
		"RAG.EmbeddingBaseURL": cfg.RAG.EmbeddingBaseURL,
		"RAG.EmbeddingAPIKey":  cfg.RAG.EmbeddingAPIKey,
		"RAG.RerankBaseURL":    cfg.RAG.RerankBaseURL,
		"RAG.RerankAPIKey":     cfg.RAG.RerankAPIKey,
		"Redis.Addr":           cfg.Redis.Addr,
		"Redis.Password":       cfg.Redis.Password,
		"Postgres.DSN":         cfg.Postgres.DSN,
		"Qdrant.Addr":          cfg.Qdrant.Addr,
		"Temporal.Host":        cfg.Temporal.Host,
		"Auth.JWTSecret":       cfg.Auth.JWTSecret,
		"Tracing.Endpoint":     cfg.Tracing.Endpoint,
		"MCP.Servers[0].URL":   cfg.MCP.Servers[0].URL,
		"MCP.Servers[0].Cmd":   cfg.MCP.Servers[0].Command,
		"MCP.Servers[0].Args0": cfg.MCP.Servers[0].Args[0],
		"MCP.Servers[0].Env.K": cfg.MCP.Servers[0].Env["K"],
	}
	for name, got := range checks {
		if got == "${EXPAND_TEST_CFG}" {
			t.Errorf("%s was NOT expanded; reflection walk missed it", name)
		}
	}
	if want := "https://filled/v1"; cfg.LLM.Primary.BaseURL != want {
		t.Errorf("LLM.Primary.BaseURL = %q, want %q", cfg.LLM.Primary.BaseURL, want)
	}
}
