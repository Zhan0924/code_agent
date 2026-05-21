package llm

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func testRouterConfig() RouterConfig {
	return RouterConfig{
		HeavyModel:  "claude-opus",
		MediumModel: "claude-sonnet",
		LightModel:  "claude-haiku",
	}
}

func TestRouter_HighComplexity_RoutesToHeavy(t *testing.T) {
	r := NewRouter(testRouterConfig(), zap.NewNop())
	route := r.Route("code_query", 8, 5)
	if route.Tier != TierHeavy {
		t.Errorf("expected heavy tier for high complexity, got %s", route.Tier)
	}
	if route.Model != "claude-opus" {
		t.Errorf("expected claude-opus, got %s", route.Model)
	}
}

func TestRouter_Deploy_RoutesToHeavy(t *testing.T) {
	r := NewRouter(testRouterConfig(), zap.NewNop())
	route := r.Route("deploy", 2, 3)
	if route.Tier != TierHeavy {
		t.Errorf("expected heavy for deploy intent, got %s", route.Tier)
	}
}

func TestRouter_SimpleConversation_RoutesToLight(t *testing.T) {
	r := NewRouter(testRouterConfig(), zap.NewNop())
	route := r.Route("conversation", 1, 3)
	if route.Tier != TierLight {
		t.Errorf("expected light for simple conversation, got %s", route.Tier)
	}
}

func TestRouter_IntentParse_RoutesToLight(t *testing.T) {
	r := NewRouter(testRouterConfig(), zap.NewNop())
	route := r.Route("_intent_parse", 0, 0)
	if route.Tier != TierLight {
		t.Errorf("expected light for intent parse, got %s", route.Tier)
	}
}

func TestRouter_CodeQuery_RoutesToMedium(t *testing.T) {
	r := NewRouter(testRouterConfig(), zap.NewNop())
	route := r.Route("code_query", 3, 5)
	if route.Tier != TierMedium {
		t.Errorf("expected medium for standard code query, got %s", route.Tier)
	}
}

func TestRouter_LongConversation_RoutesToHeavy(t *testing.T) {
	r := NewRouter(testRouterConfig(), zap.NewNop())
	route := r.Route("code_query", 5, 25)
	if route.Tier != TierHeavy {
		t.Errorf("expected heavy for long conversation, got %s", route.Tier)
	}
}

func TestRouter_Default_RoutesToMedium(t *testing.T) {
	r := NewRouter(testRouterConfig(), zap.NewNop())
	route := r.Route("unknown_intent", 4, 5)
	if route.Tier != TierMedium {
		t.Errorf("expected medium as default, got %s", route.Tier)
	}
}

func TestRouter_Stats(t *testing.T) {
	r := NewRouter(testRouterConfig(), zap.NewNop())
	r.Route("conversation", 1, 1) // light
	r.Route("deploy", 2, 1)       // heavy
	r.Route("deploy", 2, 1)       // heavy

	stats := r.Stats()
	if stats[TierLight] != 1 {
		t.Errorf("expected 1 light route, got %d", stats[TierLight])
	}
	if stats[TierHeavy] != 2 {
		t.Errorf("expected 2 heavy routes, got %d", stats[TierHeavy])
	}
}

func TestRouter_DefaultMaxTokens(t *testing.T) {
	r := NewRouter(RouterConfig{}, zap.NewNop())
	route := r.Route("conversation", 1, 1)
	if route.MaxTokens != 4096 {
		t.Errorf("expected default light max tokens 4096, got %d", route.MaxTokens)
	}
}

func TestQuickComplexity(t *testing.T) {
	tests := []struct {
		msg      string
		minScore int
	}{
		{"hello", 0},
		{"Please refactor multiple files to implement a new feature and then verify it works", 4},
	}
	for _, tt := range tests {
		score := QuickComplexity(tt.msg)
		if score < tt.minScore {
			t.Errorf("QuickComplexity(%q) = %d, want >= %d", tt.msg[:30], score, tt.minScore)
		}
	}
}

func TestRouter_RouteForMessage(t *testing.T) {
	r := NewRouter(testRouterConfig(), zap.NewNop())
	route := r.RouteForMessage(context.Background(), "deploy", "deploy to production", 5)
	if route.Tier != TierHeavy {
		t.Errorf("expected heavy for deploy, got %s", route.Tier)
	}
}
