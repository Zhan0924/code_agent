# <AUDIT-ID>: <一句话现象描述>

> 模板来源：`.claude/skills/audit-driven-fix-loop/templates/plan-template.md`
> 使用方式：复制本文件到 `docs/superpowers/plans/<YYYY-MM-DD>-<AUDIT-ID>-<slug>.md`，把所有 `<...>` 占位符替换掉，不留空。

**Audit ID:** `<AUDIT-ID>`
**审计文档来源:** `<llmdoc/.../*-audit.md>:<行号>`
**优先级:** `<P0 | P1 | P2>`
**Goal:** `<本次修复的一句话目标，可粘贴文档里的"现象"列原文>`

---

## 1. Step 1 复核结论（必填）

**判定：** `[真实存在 | 已修复 | 表述不准]`

**证据：**
- `<path>:<line>` — `<一句话说明该处实际代码状态>`
- `<path>:<line>` — `<...>`

**与审计文档原述的差异：**
`<如果是"已修复"或"表述不准"，写明差异；如果是"真实存在"写 N/A>`

---

## 2. 技术方案

### 2.1 受影响代码

| 文件 | 函数 / 方法 | 变更类型 |
|---|---|---|
| `<path>` | `<func>` | `<新增 / 修改 / 删除>` |

### 2.2 接口 / 数据流变更

`<逐项列出对外接口签名变化、调用链调整、新增 endpoint、新增 background goroutine 等。如无对外变更写 "纯内部重构，无对外接口变更"。>`

### 2.3 数据库变更（如有）

`<schema 变更 SQL / 索引调整 / migration 文件路径。如无写 N/A。>`

### 2.4 配置项 / Feature Flag

| 配置 key | 默认值 | 说明 |
|---|---|---|
| `<code_agent.xxx>` | `<default>` | `<作用>` |

---

## 3. 可观测性增强清单（必填，无则不允许进 Step 3）

> 该清单的存在目的：保证 Step 4 用 grep / curl / psql 三件套能拿到证据。

### 3.1 新增 / 增强的日志 line

| log 位置（file:func） | 级别 | 包含字段 | Step 4 如何 grep |
|---|---|---|---|
| `<path>::<func>` | `Info` | `audit_id=<AUDIT-ID>, op=<name>, tenant.user_id, before.<k>, after.<k>, result, duration_ms` | `podman logs code-agent \| rg 'audit_id=<AUDIT-ID>.*result":"ok"'` |
| `<path>::<func>` (失败分支) | `Error` | `audit_id, op, severity=critical, error` | `rg 'audit_id=<AUDIT-ID>.*"severity":"critical"'` 应**不出现** |

### 3.2 新增 / 增强的 metric

| metric 名 | 类型 | labels | 用途 |
|---|---|---|---|
| `<code_agent_xxx_total>` | counter | `op, severity` | `<...>` |

### 3.3 新增 / 增强的 DB 字段或可校验状态

| 表 | 字段 / 行为 | Step 4 如何校验 |
|---|---|---|
| `<table>` | `<column / 行数变化 / 约束>` | `psql -c "SELECT ... WHERE ..."` 期望 `<value>` |

---

## 4. 实施步骤（Step 3 落地）

- [ ] **Step 3.1** 修改 `<file>`：`<具体修改一句话>`
- [ ] **Step 3.2** 修改 `<file>`：`<...>`
- [ ] **Step 3.3** 新增 / 修改单元测试：`<test file::test func>`
- [ ] **Step 3.4** 跑 `go test ./...` 全绿
- [ ] **Step 3.5** （如有）跑 `go vet ./...` / `golangci-lint run`
- [ ] **Step 3.6** 自检 §3 三个清单全部落地

---

## 5. 部署校验脚本（Step 4 用）

**脚本位置：** `scripts/verify-<AUDIT-ID>.sh`（基于 `.claude/skills/audit-driven-fix-loop/templates/verify-template.sh`）

**三件套覆盖：**

- [ ] API 正向：`<endpoint>` 返回 `<expected status + fields>`
- [ ] API 反向：`<edge case>` 返回 `<expected error>`
- [ ] 日志出现：`audit_id=<AUDIT-ID>` 关键 line 出现
- [ ] 日志不出现：`severity=critical` 在窗口内不出现
- [ ] DB 校验：`<table>` 中 `<field>` 满足 `<predicate>`
- [ ] 反向 DB：seed 数据已清理

---

## 6. 回滚策略

| 风险点 | 回滚方式 |
|---|---|
| `<场景>` | `<config 改回默认 / git revert / db migration down>` |

---

## 7. 不在本次范围（unscoped）

`<列出修复过程中可能想顺手改、但本次显式拒绝的相邻问题，留作后续 AUDIT-ID。例如"Distiller 默认值调整 → 另起 AUDIT-Pn-X"。>`

---

## 8. 完成后动作（Step 5）

- [ ] `safe-push`：commit message `fix(audit): <AUDIT-ID> <一句话>`
- [ ] 在 `<llmdoc/.../*-audit.md>` 把当前行 ID 标 ✅，并指向本 plan 文档相对路径
- [ ] 在 `docs/architecture/<area>.md` 追加 §N 章节（如审计文档要求），格式见 audit 文档的 §6 验证方法段
