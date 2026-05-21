# 架构文档撰写 - 会话交接单 (HANDOFF)

> 本文件是**跨会话接力**的交接单。记录已完成文档、未完项、以及写作规范。

---

## 已完成 22 / 22 篇（100%）

| # | 文件 | 覆盖代码包 | 字节 | 状态 |
|---|---|---|---|---|
| 00 | overview.md | 系统总览 | ~12K | ✅ 已更新（非功能特性表） |
| 01 | config.md | `internal/config` | 9.6K | ✅ |
| 02 | models.md | `internal/models` | 12K | ✅ |
| 03 | llm.md | `internal/llm` | ~15K | ✅ **+ 跨副本熔断器 §4.3** |
| 04 | rag.md | `internal/rag` | ~22K | ✅ **+ 真实 BM25 §6.4** |
| 05 | sandbox.md | `internal/sandbox` | ~18K | ✅ **+ 深度防御 HostConfig §7.4-7.6** |
| 06 | mcp.md | `internal/mcp` | 16K | ✅ |
| 07 | tools.md | `internal/tools` | 12K | ✅ |
| 08 | skill.md | `internal/skill` | 16K | ✅ |
| 09 | orchestrator.md | `internal/orchestrator` | ~28K | ✅ **+ Intent cache 回归 §3.1、Edit 并发 §9.3.1、Diff §9.3.2** |
| 10 | planner.md | `internal/planner` | 17K | ✅ |
| 11 | temporal.md | `internal/temporal` | ~20K | ✅ **§6 重写 — Worker 已接入** |
| 12 | session.md | `internal/session` | 15K | ✅ |
| 13 | context.md | `internal/context` | 16K | ✅ |
| 14 | workspace.md | `internal/workspace` | ~17K | ✅ **+ 租户隔离 §5.1.1** |
| 15 | indexer_repomap.md | `internal/indexer` + `internal/repomap` | 16K | ✅ |
| 16 | store.md | `internal/store` + `auth/redis_revocation` | 16K | ✅ |
| 17 | api.md | `internal/api` + `cmd/agent/main.go` | ~18K | ✅ |
| 18 | auth_security.md | `internal/auth` + `internal/security` | ~20K | ✅ **+ APIKeyStore §3.3、HMAC 矩阵、Egress Transport §6.4** |
| 19 | observability.md | `internal/metrics` + `tracing` + `errors` + `pool` | 19K | ✅ |
| 20 | deploy.md | Dockerfile / compose / k8s / CI | 22K | ✅ |
| 21 | conclusion.md | 全景回顾 + 路线图 | 16K | ✅ |
| **22** | **recent_improvements.md** | **近期修复汇总**（P0/P1） | **~20K** | ✅ **新增 (2026-05)** |
| — | ARCHITECTURE_DIAGRAM.md | Mermaid / ASCII 数据流 | 31K | ✅ |

**总计**：22 篇正文 + 1 张全景图 + 1 份 API 测试指南（`../API_TEST_GUIDE.md`）。

---

## 本轮（2026-05）更新内容

所有更新来自一组 P0/P1 代码修复，**每处修改都反映了代码实际变化**。

| 主题 | 文档 | 对应代码修复 |
|---|---|---|
| Temporal worker 真接入 | 11 §6 | P0 #17 — `main.go:startTemporalWorker` |
| BM25 真实现 | 04 §6.4 | P0 #18 — `rag/bm25.go` 新增 |
| Sandbox 硬化 | 05 §7.4-7.6 | P0 #8, #9 — `sandbox/manager.go:buildHostConfig`、stdcopy demux |
| HMAC timestamp 必填 | 18 §5 | P0 #5 — `security/hmac.go` |
| API Key SHA-256 | 18 §3.3 | P0 #4 — `auth/jwt.go` |
| Egress SSRF 两层防御 | 18 §6.4 | P0 #6 — `security/egress.go` 新增 EgressTransport |
| Shared breaker | 03 §4.3 | P0 #21 — `llm/shared_breaker.go` 新增 |
| Intent cache HITL 回归 | 09 §3.1 | P0 #12 — `orchestrator.intentCacheKey` |
| EditEngine per-path lock | 09 §9.3.1 | P0 #13 — `edit_engine.go` |
| Diff hunk 算法 | 09 §9.3.2 | P0 #16 — `generateUnifiedDiff` |
| Workspace 租户隔离 | 14 §5.1.1 | P0 #15 — `ResolveSessionWorkspace` |
| **汇总清单** | **22（新增）** | 全部 P0/P1 — 单点入口 |
| 非功能特性表 | 00 §4 | 新增 7 行交叉链接 |

---

## 写作规范（给下次会话参考）

- 代码清单（文件 + 行数）放在文档顶部
- Mermaid / ASCII 图优先于纯文字
- `★` 标高亮核心概念
- 设计权衡表 15-20 行
- 末尾"后续演进 checklist" 10-15 条
- 修复类更新要加 `> ⚠️ 2026-05 更新（P0 #NN 修复）` 引用块，说明**修复前的错误行为**
- 链接优先用**相对路径**（`22_recent_improvements.md#a1`）而不是绝对路径

---

## 当前已知未完成项

见 [`22_recent_improvements.md` § F](22_recent_improvements.md#f-未完成项p1-待办) —
P1 接线类 TODO（Redis 限流接入 middleware、Egress 注入 LLM/MCP 客户端、
Streaming 熔断、tiktoken 替换、shutdown drain、默认 SSRF）。

这些是代码层的接线工作，**不是文档缺口**——完成后**同步更新对应 docs** 即可。
