# Code Agent 优化路线图 (Roadmap)

## 已完成的优化 ✅

### V1.0 — 基础能力
- [x] ReAct 多步推理循环
- [x] 文档先行 4 阶段工作流 (Plan → Code → Integration Test → Spec Verify)
- [x] 进程组超时杀死 (SysProcAttr + process group kill)
- [x] 集成测试端口隔离 (19090 vs 8080/18080)

---

## P0 — 高优先级 ✅ 全部完成

### 1. ✅ 多轮任务续接 (Task Continuation)
- [x] 达步数上限时，`saveProgressForContinuation()` 写入 `.progress.json`
- [x] 返回结构化响应引导用户说 "continue" 续接
- [x] **[NEW] Auto-continue**: 检测 "continue"/"继续"/"go on" 消息，自动读取 `.progress.json` + `.plan.md` + workspace file tree 注入上下文恢复 (`buildContinuationPrompt`)

### 2. ✅ LLM 故障重试 + 进度保存
- [x] ReAct 循环中 LLM 调用失败时自动重试 3 次（指数退避 2s, 4s）
- [x] 3 次重试全部失败后，保存 `.progress.json` 并返回可续接的错误消息

### 3. ✅ 工具结果智能摘要化
- [x] `smartTruncateOutput()`: 超长输出保留 HEAD 1/3 + TAIL 2/3
- [x] `extractTestSummary()`: Go test -v 输出自动提取 PASS/FAIL 摘要 + 失败详情
- [x] 全局工具结果截断: HEAD 8K + TAIL 12K（orchestrator 层）

---

## P1 — 重要 ✅ 全部完成

### 4. ✅ 流式进度推送
- [x] SSE 端点 `/chat/stream` 已有完整实现
- [x] ReAct 循环每步通过日志输出进度

### 5. ✅ Workspace 隔离
- [x] `ResolveSessionWorkspace(sessionID)` 每 session 独立 workspace
- [x] 使用 session ID 作为 workspace ID，防并发冲突

### 6. ✅ 智能 Patch 优先策略
- [x] write_file 描述更新："For files > 100 lines, PREFER patch_file"

---

## P2 — 锦上添花 ✅ 部分完成

### 7. ✅ 依赖自动管理
- [x] `autoDepManagement()`: .go → go mod tidy, package.json → npm install, requirements.txt → pip install

### 8. 工具结果缓存 — 待实现
- [ ] read_file 缓存 + go build 缓存

### 9. 多模型协作 — 待实现
- [ ] 轻量 LLM 做工具结果摘要 + 不同模型做不同任务

### 10. ✅ 自适应步数限制
- [x] `getMaxSteps(intent)`: code_query=10, code_execute=20, diagnose=25, mcp_call=15, deploy=20, conversation=50

### 11. 质量自评估 — 待实现
- [ ] Phase 5: 独立 LLM 做 code review

---

## 新增：高级复杂任务优化 ✅

### Fix-1: ✅ Auto-Continue 上下文恢复
**问题**: 用户发 "continue" 时 LLM 丢失了早期上下文，不知道该从哪里继续  
**实现**: `buildContinuationPrompt()` 自动读取 workspace 中的：
- `.progress.json` — 保存的进度状态
- `.plan.md` — 技术方案和执行清单（含 checkbox 进度）
- workspace file tree — 当前文件列表
所有内容注入为丰富的上下文消息，引导 LLM 从断点精确恢复

### Fix-2: ✅ 定期反思 (Reflection Checkpoint)
**问题**: 30+ 步后 LLM 偏离计划，不再对照 .plan.md 执行  
**实现**: `reflectionCheckpoint()` 每 10 步注入一条系统消息：
- 提醒 LLM 读取 .plan.md 检查进度
- 评估是否偏离计划，是否有未修复的错误
- 根据剩余步数优先处理最关键的任务

### Fix-4: ✅ 修复循环限制器 (Fix Loop Limiter)
**问题**: LLM 对同一个编译错误反复尝试相同修复，浪费 5-10 步  
**实现**: `consecutiveFailureTracker` 追踪连续失败：
- 同一工具连续失败 3 次 → 注入 "STEP BACK" 系统消息
- 强制 LLM 停止重复，重新分析错误根因
- 建议不同的修复策略（如从 patch 改为完整重写）
- 工具切换或成功时自动重置计数器

---

## 技术债务 ✅ 关键项已修复

| 债务 | 状态 | 修复详情 |
|------|------|----------|
| pruneMessages 固定 keep | ✅ 已修复 | 动态计算：累加 token 到 60% budget |
| 单元测试覆盖率 | ✅ 已修复 | 13 个测试覆盖所有新功能 |
| token 估算近似 | 🔧 低优先级 | len()/4 精度足够 |
| Intent 分类兜底 | 🔧 低影响 | Document-First 正常工作 |

---

## 验证结果

| 测试类型 | 结果 |
|----------|------|
| `go build ./...` | ✅ 编译通过 |
| 单元测试 (13 tests) | ✅ 全部通过 |
| Docker 镜像构建 | ✅ 构建成功 |
| **E2E 综合测试 (29 tests)** | ✅ **29/29 全部通过** |

---

## 架构能力总览

```
┌─────────────────────────────────────────────────────────────┐
│                    Code Agent V1.2                            │
├─────────────────────────────────────────────────────────────┤
│ ReAct Loop (最多 50 步, 自适应)                               │
│  ├── Document-First 4-Phase Workflow                         │
│  ├── Auto-Continue with Context Recovery    [Fix-1] ✅       │
│  ├── Reflection Checkpoint (每 10 步)       [Fix-2] ✅       │
│  ├── Fix Loop Limiter (连续 3 次失败)       [Fix-4] ✅       │
│  ├── LLM 3x Retry + Exponential Backoff    [P0-2]  ✅       │
│  ├── Smart Output Truncation (HEAD+TAIL)    [P0-3]  ✅       │
│  └── Dynamic Context Pruning (60% budget)   [Debt]  ✅       │
├─────────────────────────────────────────────────────────────┤
│ Workspace (per-session isolation)           [P1-5]  ✅       │
│  ├── Auto Dep Management (go/npm/pip)       [P2-7]  ✅       │
│  └── Smart Patch Priority                   [P1-6]  ✅       │
├─────────────────────────────────────────────────────────────┤
│ Infrastructure                                               │
│  ├── Docker Sandbox + Process Group Kill                     │
│  ├── SSE Streaming                                           │
│  ├── JWT Auth + Rate Limiting                                │
│  ├── PostgreSQL Persistence + Redis Sessions                 │
│  ├── Qdrant RAG + AST Parsing                                │
│  ├── MCP Gateway (GitHub/Jira/etc)                           │
│  ├── OpenTelemetry Tracing + Prometheus Metrics              │
│  └── Temporal Workflows (HITL)                               │
└─────────────────────────────────────────────────────────────┘
```
