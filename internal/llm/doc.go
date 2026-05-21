// Package llm 封装对大语言模型（LLM）的统一调用接口，支持多供应商、熔断、重试与降级。
//
// # 设计目标
//
// 生产级 Agent 不能被单一 LLM 供应商绑死，也不能在对方 API 波动时整体挂掉。
// 本包抽象出以下 4 个能力：
//
//  1. Provider 抽象：OpenAI / Anthropic / 本地 Ollama 统一接口 (Chat, Embed, Stream)
//  2. Circuit Breaker：按 provider 维度独立熔断（closed → open → half-open）
//  3. Fallback Router：primary 不可用时自动切到 backup 或本地模型
//  4. 指数退避重试：幂等请求（Embed）可重试；有副作用请求（Chat）限次数
//
// # 三态熔断器
//
//	Closed    —— 正常放行
//	Open      —— 累计失败率 > 阈值 (默认 50% in 30s)，直接拒绝请求 60 秒
//	HalfOpen  —— Open 冷却后允许 1 个试探请求；成功则 Closed，失败则回 Open
//
// 状态转换由 config.CircuitBreakerConfig 参数化。指标发射到
// metrics.LLMCircuitBreakerState（0=Closed, 1=HalfOpen, 2=Open）。
//
// # Fallback 路由
//
// Router 在 primary provider 熔断或 token 超限时，按配置顺序尝试 secondary：
//
//	llm:
//	  primary:   { provider: openai,    model: gpt-4o }
//	  secondary: { provider: anthropic, model: claude-3-5-sonnet }
//	  local:     { provider: ollama,    model: llama3 }
//
// 每次降级会 +1 llm_fallback_total 指标；运维依此判断上游健康度。
//
// # Streaming
//
// Chat 支持流式（SSE）：Provider.ChatStream 返回 <-chan StreamChunk，Orchestrator
// 把每块以 SSE 格式 flush 到 HTTP 响应。背压由 channel 缓冲控制。
//
// # 关键类型
//
//	Client           —— 面向调用方的主入口（带熔断、重试、fallback）
//	Provider         —— 供应商接口 (OpenAI / Anthropic / Local)
//	Router           —— 多 provider 智能路由
//	openAIProvider   —— OpenAI 实现（含 tool_calls 协议适配）
//
// # 用法示例
//
//	client := llm.New(cfg.LLM, logger)
//	resp, err := client.Chat(ctx, llm.ChatRequest{
//	    Messages: msgs,
//	    Tools:    toolDefs,
//	    Stream:   false,
//	})
//
// 详见 docs/architecture/03_llm.md。
package llm
