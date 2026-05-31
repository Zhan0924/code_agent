package orchestrator

import (
	"github.com/agent/code_agent/internal/llm"
)

// SetRouter wires the optional model-tier Router into the orchestrator.
// When set, every LLM call inside the orchestrator (intent parsing, ReAct
// loop, streaming chat) consults the router to pick Heavy/Medium/Light
// tier before issuing the request. nil = router disabled (no model override
// applied; the request uses the LLM client's default model).
func (o *Orchestrator) SetRouter(r *llm.Router) {
	o.llmRouter = r
}

// applyModelRoute is the single hook all ChatCompletion / ChatCompletionStream
// call sites use to consult the router. It mutates req.Model and req.MaxTokens
// based on the routing decision; when the router is nil it's a no-op so the
// upstream caller's defaults survive verbatim.
//
// intent is one of the canonical task intents ("conversation", "code_query",
// "_intent_parse", ...) used by router.go's classifier. message is the raw
// user input — fed into the lightweight complexity heuristic. messageCount
// influences the "long conversation" rule (>20 → Heavy).
func (o *Orchestrator) applyModelRoute(req *llm.ChatRequest, intent, message string, messageCount int) {
	if o == nil || o.llmRouter == nil || req == nil {
		return
	}
	route := o.llmRouter.Route(intent, llm.QuickComplexity(message), messageCount)
	o.llmRouter.ApplyRoute(req, route)
}
