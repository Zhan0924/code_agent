# 端到端示例：用 audit-driven-fix-loop 闭环 AUDIT-P2-5

> 用真实已闭环的 `AUDIT-P2-5`（memory 可解释性 explain endpoint）演示完整五步循环。
> 真实代码已经合入，本文件只演示"如果你今天接手这条 AUDIT，应该怎么按 skill 跑"。

## 0. 起点

审计文档：`llmdoc/memory/reflections/2026-06-26-memory-system-audit.md` 第 §3.3 表中 `AUDIT-P2-5`：

> 可解释性缺失。用户问"agent 为什么知道我用 tabs"，没有 audit trail 显示注入了哪条 memory ID + 何时蒸馏。

复制进度卡：

```
当前问题：AUDIT-P2-5  来源：llmdoc/memory/reflections/2026-06-26-memory-system-audit.md:61
- [ ] Step 1  代码复核
- [ ] Step 2  Plan
- [ ] Step 3  实现
- [ ] Step 4  Docker 三件套校验
- [ ] Step 5  safe-push + 勾 ✅
```

---

## 1. Step 1 · 代码复核

工具调用顺序：

1. 先查 `codebase-memory-mcp` 的 `search_graph`：`{"project":"Users-zhanqiankun.1-code-code_agent","query":"buildDynamicMemory"}` → 找到 `internal/orchestrator/memory_bridge.go::buildDynamicMemory`
2. `Read` 上述函数 → 看是否注入 `mem_ids` 字段、log 级别
3. `Grep` `/api/v1/memory/explain` → 看是否已有 endpoint

判定输出：

```
判定：真实存在
证据：
  - internal/orchestrator/memory_bridge.go:142 — buildDynamicMemory 只打 Debug log，不带 mem_ids 字段
  - internal/api/memory_handlers.go：无 GET /memory/explain/:id 路由
  - internal/api/router.go：无 explain 路由注册
结论：进入 Step 2
```

---

## 2. Step 2 · Plan 模式生成方案

调用 `SwitchMode` → `plan`。

新建 `docs/superpowers/plans/2026-06-29-AUDIT-P2-5-explain-endpoint.md`，按 `templates/plan-template.md` 填写。关键字段：

- **受影响代码**：
  - `internal/orchestrator/memory_bridge.go::buildDynamicMemory`（升级日志 Debug → Info，新增 `mem_ids` 字段）
  - `internal/api/memory_handlers.go`（新增 `GetMemoryExplain` handler）
  - `internal/api/router.go`（注册 `GET /api/v1/memory/explain/:id`）
- **可观测性增强**（必填）：

| log 位置 | 级别 | 字段 | Step 4 grep |
|---|---|---|---|
| `memory_bridge.go::buildDynamicMemory` | `Info` | `audit_id=AUDIT-P2-5, op=memory_inject, mem_ids=[...], turn_id` | `rg 'audit_id=AUDIT-P2-5.*mem_ids'` |
| `memory_handlers.go::GetMemoryExplain` | `Info` | `audit_id=AUDIT-P2-5, op=memory_explain, id, found` | `rg 'audit_id=AUDIT-P2-5.*op":"memory_explain"'` |

- **回滚策略**：endpoint 是只读的，无 db migration；revert commit 即回滚。

---

## 3. Step 3 · 实现

修改三个文件，关键示意（不是完整代码）：

```go
// internal/orchestrator/memory_bridge.go
logger.Info("memories injected into prompt",
    zap.String("audit_id", "AUDIT-P2-5"),
    zap.String("op", "memory_inject"),
    zap.Strings("mem_ids", memIDs),
    zap.String("turn_id", turnID),
    zap.Int("count", len(memIDs)),
)
```

```go
// internal/api/memory_handlers.go
func (h *MemoryHandler) GetMemoryExplain(c *gin.Context) {
    id := c.Param("id")
    start := time.Now()

    mem, err := h.store.GetByID(c.Request.Context(), id)
    if err == ErrNotFound {
        h.logger.Info("memory explain not found",
            zap.String("audit_id", "AUDIT-P2-5"),
            zap.String("op", "memory_explain"),
            zap.String("id", id),
            zap.Bool("found", false),
        )
        c.JSON(404, gin.H{"error": "not found"})
        return
    }
    if err != nil { /* 500 + error log */ }

    h.logger.Info("memory explain ok",
        zap.String("audit_id", "AUDIT-P2-5"),
        zap.String("op", "memory_explain"),
        zap.String("id", id),
        zap.Bool("found", true),
        zap.Duration("duration_ms", time.Since(start)),
    )
    c.JSON(200, mem)
}
```

```go
// internal/api/router.go
v1.GET("/memory/explain/:id", memHandler.GetMemoryExplain)
```

补一个单元测试 `TestBuildDynamicMemory_AuditLogAndCitations` 锁定字段契约。`go test ./internal/orchestrator/... ./internal/api/...` 全绿。

---

## 4. Step 4 · Docker 部署 + 三件套

### 4.1 镜像 + 依赖检查

```bash
podman ps --format '{{.Names}}\t{{.Status}}' | rg 'agent-(postgres|qdrant|temporal|redis)'
# 假设：agent-postgres Up / agent-qdrant Up / agent-temporal Up → 不重启依赖

podman images --format '{{.Repository}}:{{.Tag}}' | rg 'pgvector/pgvector:pg16'
# 命中 → 跳过 pull
```

### 4.2 build + 重启 agent

```bash
cd /Users/zhanqiankun.1/code/code_agent
podman build -t code-agent:latest .
podman rm -f code-agent 2>/dev/null || true
podman compose up -d agent
```

健康检查循环（reference.md §2.3）等到 `/healthz=200`。

### 4.3 verify 脚本

直接复用 `scripts/verify-audit-p2-5.sh`（项目里已有，本 skill 的 `templates/verify-template.sh` 就是它的抽象版）：

```bash
./scripts/verify-audit-p2-5.sh
```

期望输出：

```
[verify-p2-5] 1/4 healthz
[verify-p2-5] 2/4 seed an explainable memory
[verify-p2-5]     seeded id=55555555-1111-4111-a111-555555555501
[verify-p2-5] 3/4 GET /api/v1/memory/explain/<known-id>
[verify-p2-5]     explain returned full row; content matches
[verify-p2-5] 4/4 GET /api/v1/memory/explain/<unknown-id> returns 404
[verify-p2-5]     unknown id correctly returns 404
[verify-p2-5] PASS: AUDIT-P2-5 docker verification complete
```

补充三件套覆盖检查（按 `reference.md::"三件套覆盖矩阵"`）：

- ✅ 健康检查：脚本 1/4
- ✅ API 正向：脚本 3/4（200 + 字段断言）
- ✅ API 反向：脚本 4/4（404）
- ✅ 日志出现：本案例额外加一段 `podman logs --since 2m code-agent | rg 'audit_id=AUDIT-P2-5.*"op":"memory_explain"'`
- ✅ 日志不出现：`! podman logs --since 2m code-agent | rg 'audit_id=AUDIT-P2-5.*"severity":"critical"'`
- ✅ DB 校验：脚本通过 `INSERT` + `SELECT` 隐式做了，明确化的话再加 `SELECT count(*) FROM memories WHERE id=...`
- ✅ 清理 seed：脚本最后 `DELETE FROM memories WHERE id='${MEM_ID}'`

任一失败 → 回 Step 3 改。

---

## 5. Step 5 · safe-push 提交

`git status` 期望只看到本次相关的：

```
 M internal/api/memory_handlers.go
 M internal/api/router.go
 M internal/orchestrator/memory_bridge.go
 M llmdoc/memory/reflections/2026-06-26-memory-system-audit.md   # 勾 ✅
?? docs/superpowers/plans/2026-06-29-AUDIT-P2-5-explain-endpoint.md
?? scripts/verify-audit-p2-5.sh
?? internal/orchestrator/memory_audit_log_test.go
```

调用 `/safe-push`，commit message：

```
feat(audit): AUDIT-P2-5 add memory explain endpoint + structured audit log

- Expose GET /api/v1/memory/explain/:id for single-memory traceback
- Upgrade buildDynamicMemory log to Info with mem_ids[] for prompt-injection audit trail
- Add verify-audit-p2-5.sh covering API positive/negative + log + DB assertions
- Mark AUDIT-P2-5 ✅ in memory-system-audit.md → see plans/2026-06-29-AUDIT-P2-5-explain-endpoint.md
```

---

## 6. 回到 Step 1 处理下一条

完成 AUDIT-P2-5 后，**不**继续在同一笔提交里改 AUDIT-P2-4，而是回到本 skill 的 Step 1，针对 AUDIT-P2-4（错误分级 + DLQ）重新走一遍五步循环。
