// Package context 负责把"系统提示 + RAG 上下文 + 历史消息 + 工具定义"装配成 LLM
// 可识别的 Prompt，并做智能压缩。
//
// 包名说明：为避免与标准库 context 冲突，调用方通常起别名：
//
//	import agentctx "github.com/agent/code_agent/internal/context"
//
// # PromptBuilder
//
// 单次 /chat 的 Prompt 由以下部分按**稳定顺序**拼接（顺序稳定是 KV Cache 复用的前提）：
//
//	┌──────────────────────────────┐
//	│ 1. System Prompt             │ ← 角色描述、全局规则（项目级 + 用户级）
//	├──────────────────────────────┤
//	│ 2. Tool Definitions (JSON)   │ ← 可用工具的 schema（含 MCP 动态注册的）
//	├──────────────────────────────┤
//	│ 3. Persistent Memory         │ ← project_rules / user_preferences（少变）
//	├──────────────────────────────┤
//	│ 4. RAG Context (Top 5)       │ ← rag.Retrieve 返回的代码片段
//	├──────────────────────────────┤
//	│ 5. Conversation History      │ ← session.Manager 提供的滑动窗口消息
//	├──────────────────────────────┤
//	│ 6. User Message (current)    │ ← 本轮用户输入
//	└──────────────────────────────┘
//
// # KV Cache 友好性
//
// 大模型（GPT/Claude）会复用 Prompt 的公共前缀 KV 值以降低 TTFT（首 token 延迟）。
// 本包通过以下手段最大化 KV Cache 命中率：
//
//   - 固定段顺序：越不变的放越前
//   - 稳定序列化：工具 JSON 按字段名排序
//   - 批 hash 记录：每层前缀的 SHA256 存入 metrics，便于诊断命中率
//
// # Pruner（上下文压缩）
//
// pruner.go 实现基于优先级的渐进式压缩：
//
//	优先级（从低到高，先丢低的）：
//	  1. 过往的 tool_result（只保留最近 2 轮）
//	  2. 中间 assistant 消息的冗余思考过程
//	  3. RAG 片段里未被后续引用的部分
//	  4. 更老的 user-assistant 对（交给 Summarizer 摘要）
//
// 保底：system prompt 和 tools 从不压缩。
//
// # 关键类型
//
//	PromptBuilder  —— BuildPrompt(sys, rag, history, tools) → []Message
//	Pruner         —— Prune(messages, targetTokens) → prunedMessages
//	TokenEstimator —— 快速 token 估算（避免每次调 tiktoken 的开销）
//
// 详见 docs/architecture/13_context.md。
package context
