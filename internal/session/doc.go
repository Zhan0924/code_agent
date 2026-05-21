// Package session 管理用户多轮对话的会话状态，采用"滑动窗口 + 冷归档"策略。
//
// # 为什么需要 Session
//
// LLM 本质无状态：每次对话必须把历史消息全部传入。若不做管理，随轮数增长 token
// 很快超出模型上下文窗口（4k / 32k / 128k），同时成本线性升高。
//
// 本包提供：
//
//   - 热存储：Redis List，O(1) append，低延迟读取最近 N 条
//   - 冷存储：PostgreSQL，长期归档，按需恢复
//   - 滑动窗口：保留最近 M 条原始消息 + 系统摘要 + system prompt
//   - 自动摘要（Summarizer）：超限时异步调轻量 LLM 压缩老消息
//   - 冷热迁移：>7d 不活跃 session 自动归档
//
// # 数据分布
//
//	Redis:
//	  session:{id}:meta       HSET user_id/created/updated/ttl
//	  session:{id}:messages   LIST (RPUSH / LRANGE) 近 50 条
//	  session:{id}:summary    STRING 累积摘要（~300 token）
//
//	PostgreSQL:
//	  sessions                id, user_id, created_at, archived_at
//	  messages                session_id, role, content, tool_calls, created_at
//
// # 滑动窗口算法
//
//  1. 触发条件：history_tokens + new_message_tokens > threshold (默认 4000)
//  2. 保留：system prompt + 最近 10 条消息
//  3. 压缩：中间老消息 → Summarizer (gpt-4o-mini or claude-haiku)
//  4. 替换：旧的"中间段"替换为单条 system 消息 "以下为历史摘要：..."
//  5. 结果：4000+ tokens → ~1500 tokens
//
// # 关键类型
//
//	Manager     —— 对外入口：CreateSession / AppendMessage / GetHistory / Archive
//	Summarizer  —— LLM 驱动的摘要器（summarizer.go），支持 retry + fallback
//
// # 并发安全
//
// Redis 操作天然原子（LPUSH/LRANGE）；同一 session 的读-改-写通过 Redis
// 分布式锁（SET NX PX）保护，防止多副本 Agent 并发摘要冲突。
//
// 详见 docs/architecture/12_session.md。
package session
