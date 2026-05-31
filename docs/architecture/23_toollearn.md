# 23 · 工具使用学习系统 `internal/toollearn`

> 代码：
> - `types.go` (40) — `Feedback` / `ToolPattern` / `Advice` / `Store` 接口
> - `collector.go` (95) — 工具反馈收集（内存 buffer + 可选 Postgres 持久化）
> - `extractor.go` (108) — 从 feedback 中提取 ToolPattern（失败率、常见错误、平均耗时）
> - `advisor.go` (70) — 工具调用前的预警 / 提示
> - `distiller.go` (298) — 蒸馏成功的工具链为 `StrategyEntry` 复用
> - `policy.go` (333) — `AdaptivePolicy` 动态工具排序 + 序列推荐
> - `pg_store.go` (105) — `Store` 接口的 Postgres 实现
>
> 测试：`policy_test.go` (170) / `distiller_test.go` (180)

---

## 1. 模块定位

**"看 agent 哪些工具用得顺、哪些容易翻车，把'经验'喂回给 LLM。"**

`toollearn` 是一个**经验积累闭环**：

```
Tool 执行 ─→ Collector.Record(...) ─→ buffer/PG
                ↓
            Extractor 提取 ToolPattern
            Distiller 提取 StrategyEntry
            Policy 排序 + 序列推荐
                ↓
LLM 调用前 ─→ Advisor.Advise(tool) ─→ "[Tool Learning Warning] ..."
              注入 system prompt
```

实际效果：

- 用户第一次让 agent 改 React 组件，`write_file` 多次失败（路径不对）。Collector 记录失败 + 错误信息。
- 用户第二次让 agent 改 React 组件时，`Advisor.FormatForLLM("write_file")` 返回：

```
[Tool Learning Warning] Tool "write_file" has a 67% failure rate (last 12 calls).
Common errors: [no such file or directory, permission denied]
```

LLM 拿到这条 system 消息，会**先 list_dir 确认路径再 write_file**。

这是**工具调用层面的"自学习"**，与 [21_agentloop](21_agentloop.md) 的 `AdaptiveFeedback` 是同一思路的**长期化**：

| 维度 | agentloop.AdaptiveFeedback | toollearn |
|------|-----------------------------|-----------|
| 作用域 | 单次 ReAct 循环内 | 跨 session、跨 process |
| 存储 | 内存 + 循环结束即丢 | 内存 + Postgres |
| 反馈触发 | 工具失败后注入 hint | 工具调用前注入 advice |
| 学习数据 | 最近 10 次错误 | 数日/数周的全量 |

两者**互补**：短反馈让当下别犯同一错；长反馈让 LLM 一上来就知道哪些工具靠不住。

---

## 1.5 核心设计问题

### 为什么不直接让 LLM 自己看 token usage 来"学习"？

LLM 没有持久记忆。同一个用户 session 内 LLM 也许能"记住失败"，但 session 结束就丢。`toollearn` 的目的就是**把工具反馈持久化**：

- Collector 写 PG（`tool_feedback` 表，索引 tool_name + created_at）；
- Extractor / Policy 在内存里跑统计，启动时 cold start 也行；
- Advisor 把"过去 7 天的经验"翻译成 LLM 看得懂的 system 提示。

→ 这条流水线让 agent 跨重启依然记得"上周 write_file 总挂"。

### 为什么有 4 个产物（pattern / strategy / advice / score）？

它们处于不同的**抽象层级**：

| 名字 | 输入 | 输出 | 用途 |
|------|------|------|------|
| `ToolPattern` | 单工具的近 50 条 feedback | `{failure_rate, avg_duration, common_errors[]}` | Advisor 用来发警告 |
| `StrategyEntry` | 整个 session 的 feedback 序列 | `{tool_chain[], success_rate, task_pattern}` | Distiller 用来推荐工具链 |
| `Advice` | 单工具名 | `{warning, hint}` | LLM 看到的最终文本 |
| `toolScore` | 全量 feedback + 序列 | `{success_rate, trend, avg_duration}` | Policy 用来给工具排序 |

四个东西**视角不同**：pattern 看"工具好不好"，strategy 看"工具链如何走"，advice 看"对 LLM 说什么"，score 看"下一步排哪个工具靠前"。

### 为什么有内存 buffer 又有 Postgres？

内存 buffer（`maxBuf=1024`）是热路径：每次工具调用都 push，统计的时候不走 IO。Postgres 是冷路径，**仅当 `store != nil` 才写**——开发环境 / 测试用例可以 `NewCollector(nil, logger)` 完全内存化。

进程重启时不会去 PG 回灌内存 buffer（这是当前的弱点，见 §12 演进列表）；目前依赖**长期运行**积攒数据。

---

## 2. 依赖架构

```
                ┌────────────────────────────────────────┐
                │   Orchestrator.executeTool(call)       │
                │   ├─ pre-dispatch:                     │
                │   │  hint = advisor.FormatForLLM(...)  │ ← 注入 prompt
                │   │                                    │
                │   ├─ 实际执行                          │
                │   │                                    │
                │   └─ post-dispatch:                    │
                │      collector.Record(...)             │
                └────────┬───────────────────────────────┘
                         │ records
                         ▼
            ┌────────────────────────────┐
            │  toollearn.Collector       │
            │  ├─ buffer []Feedback      │
            │  ├─ store PGStore? (opt)   │
            │  └─ Record / Stats /        │
            │     RecentFeedback         │
            └────────┬───────────────────┘
                     │ 读取 buffer
              ┌──────┼─────────────────────┐
              ▼      ▼                     ▼
      ┌─────────┐  ┌──────────┐  ┌──────────────────────┐
      │Extractor│  │Distiller │  │  AdaptivePolicy      │
      │         │  │          │  │  ├─ scores map       │
      │ToolPat. │  │Strategy  │  │  ├─ sequences map    │
      │         │  │  Entry   │  │  └─ Update/Rank/Sugg │
      └────┬────┘  └─────┬────┘  └──────────┬───────────┘
           │             │                   │
           ▼             ▼                   ▼
      ┌─────────┐  ┌──────────────┐  ┌─────────────────┐
      │ Advisor │  │FormatRecomm. │  │FormatContextHint│
      │ Advise  │  │              │  │                 │
      └────┬────┘  └──────┬───────┘  └────────┬────────┘
           │              │                   │
           └──────┬───────┴───────────────────┘
                  ▼
        [System Prompt 注入]
        " [Tool Learning Warning] write_file 失败率 67%..."
        " [Learned Strategy] code_modification 任务通常..."
        " [Tool Learning Insights] After read_file, edit_file..."
```

---

## 2.5 数据流总览

```text
═══════════════════════════ 写入路径 (热) ═══════════════════════════

orchestrator.executeTool:
  start := Now()
  result, err := dispatch(tool, args)
  duration := Since(start)
  collector.Record(tool, args, success=(err==nil && !result.IsError),
                   duration, errMsg, sessionID)
    └─→ feedback := Feedback{ToolName, ArgsHash, Success, ...}
        buffer.append(fb)         # ≤ 1024 条 FIFO
        if store != nil:
          store.RecordFeedback(fb)   # → PG INSERT

═══════════════════════════ 分析路径 (温) ═══════════════════════════

extractor.Analyze("read_file"):
  recent := collector.RecentFeedback("read_file", 50)
  if len(recent) < 3: return nil
  counts: successes/failures/duration/errors
  → ToolPattern{FailureRate, AvgDuration, CommonErrors[err≥2]}
  patterns["read_file"] = pat

distiller.Distill():
  new := buffer[processedOffset:]
  bySession := group(new)
  for sess, feedbacks in bySession:
    if sessionRate < 0.7: skip      # 只学成功 session
    chain := tools used successfully
    pattern := classifyTaskPattern(chain)   # → "code_modification" 等
    updateStrategy(pattern, chain, feedbacks)
      └─ 新 pattern: 创建 StrategyEntry
         旧 pattern: 加权平均 (0.8 * old + 0.2 * new)
                    若新 chain 更短且成功率高 → 替换 chain

policy.Update():
  全量 buffer → updateScores + updateSequences
  scores[tool] = {SuccessRate, AvgDuration, RecentTrend, Samples}
  sequences["A→B"] = {SuccessCount, TotalCount, AvgDuration}

═══════════════════════════ 注入路径 (冷) ═══════════════════════════

每次 prompt 构建:
  msg := []
  for tool in availableTools:
    if hint := advisor.FormatForLLM(tool); hint != "":
      msg.append(hint)
  if rec := distiller.FormatRecommendation(taskHint); rec != "":
    msg.append(rec)
  if ins := policy.FormatContextHint(lastTool); ins != "":
    msg.append(ins)

  prompt = systemPrompt + "\n" + join(msg)
```

---

## 3. `Feedback` —— 数据基元

```go
type Feedback struct {
    ID        int64       // PG sequence
    ToolName  string
    ArgsHash  string      // sha256(args)[:10] —— 隐私 + 去重
    Success   bool
    Duration  int         // ms
    ErrorMsg  string
    SessionID string
    CreatedAt time.Time
}
```

设计细节：

- **ArgsHash 而不是 Args**：参数可能含敏感信息（文件路径、API key），hash 10 字节够区分但不泄露内容。代价是无法反查"具体哪个参数挂了"——这是隐私保护的权衡。
- **ErrorMsg 是裸字符串**：用 `extractor` 提取 `CommonErrors` 时按文本聚合（出现 ≥2 次的算 common）。LLM 拿到这些 raw 错误字符串后能反推出"哦，又是路径问题"。

---

## 4. `Collector` —— 热路径

```go
collector.Record(toolName, args, success, duration, errMsg, sessionID)
  buffer.append → cap 1024 FIFO
  if store != nil: err := store.RecordFeedback(fb)  // 同步 best-effort，失败仅 logger.Debug
```

**关键点**：

- **buffer 满了直接丢**（`if len(buffer) < maxBuf`）——不阻塞、不报错。1024 条在中等使用强度下覆盖约一小时，足以让 Extractor/Policy 拿到足够样本。
- **PG 写是同步 best-effort**：`Record` 在调用者 goroutine 里直接调 `store.RecordFeedback`；失败只 `logger.Debug(...)`，**不会** spawn 新 goroutine、不会重试、不返回 error。设计原则：**学习失败 ≠ 主流程失败**——但 PG 慢会拖住 tool 调用 hot path（已知缺陷，见 §10）。

`RecentFeedback(toolName, n)` 从 buffer **倒序**取最新 n 条匹配某工具。`""` 表示不过滤。

---

## 5. `Extractor` —— 工具级模式

```go
Analyze(toolName) → *ToolPattern
  recent := collector.RecentFeedback(toolName, 50)
  if len < 3: return nil       # 样本不足
  
  successes, failures, totalDuration = aggregate(recent)
  errorCounts = histogram(failures.errMsg)
  
  pattern = ToolPattern{
    FailureRate: failures/total,
    AvgDuration: totalDuration/total,
    CommonErrors: [err for err,count in errorCounts if count≥2],
    SampleSize: total,
  }
```

`AnalyzeAll()` 扫描 buffer 内所有 tool name 各跑一次——通常由后台 ticker 周期调用（**当前没接 ticker**，由 Advisor lazy 触发）。

`FailureSequences(toolName, minStreak)` 还能拿"连续失败的时间点"——这个 API 当前未在 orchestrator 中接入，预留给 dashboards。

---

## 6. `Distiller` —— 会话级蒸馏

`distiller.Distill()` 把**成功的 session**（成功率 ≥70%）的工具序列提取为 `StrategyEntry`。

### 6.1 任务分类（hardcoded）

```go
classifyTaskPattern(chain) string
  hasRead = any(tool contains "read")
  hasWrite = any(contains "write"|"edit"|"patch")
  hasTest = any(contains "test")
  
  switch:
    read+write+test: "implement_and_verify"
    read+write:      "code_modification"
    read only:       "code_analysis"
    test only:       "testing"
    default:         "general"
```

**为什么写死 5 类？** 真实任务的复杂度远超 5 类，但只要分类够粗就能跟用户意图大致对齐。`patternRelevance(pattern, taskHint)` 用关键词匹配做软关联（"implement"/"add"/"feature" → implement_and_verify）。

### 6.2 推荐文本

```go
distiller.FormatRecommendation("帮我修复登录 bug")
// 触发 patternRelevance("code_modification", "...修复...") = 1.0
// 返回：
// [Learned Strategy] For code_modification tasks, the tool sequence
// read_file → edit_file → run_tests has 85% success rate (12 samples).
```

注意：**只有 `SampleCount ≥ minSamples=5` 的 strategy 才会被推荐**——避免一次成功就被当作"经验"。

### 6.3 增量学习

Distiller 维护 `processedOffset`，每次 `Distill()` 只处理新增的 feedback。`updateStrategy` 用 EMA（0.8 老 + 0.2 新）平滑成功率，避免单次异常拉跑数据。

特殊优化：**短而成功的 chain 会替换长 chain**：

```go
if len(chain) < len(existing.ToolChain) && newRate >= 0.8 {
    existing.ToolChain = chain   # 学到更高效的工具链
}
```

这是"少即是多"的偏好——同样成功的工具链，5 步胜过 8 步。

---

## 7. `AdaptivePolicy` —— 动态排序

### 7.1 双维度学习

```go
scores    map[toolName]*toolScore       # 单工具
sequences map[A→B]*sequenceStats        # 工具对
```

`toolScore` 包含 **trend**：

```go
trend = secondHalfRate - firstHalfRate   # 末尾 25 vs 起始 25
```

正值表示工具最近变好（环境变化？参数调对了？），负值表示在恶化。

### 7.2 RankTools —— 工具菜单重排

```go
RankTools(toolNames, lastTool):
  for tool in toolNames:
    if no score data: score = 0.5
    else:
      score = ts.SuccessRate
      score += ts.RecentTrend * 0.1     # 趋势加成
      if lastTool != "":
        key = lastTool + "→" + tool
        if seq exists:
          score += (seqRate - 0.5) * 0.2  # 序列加成
  sort by score desc
```

这个函数**当前没被 orchestrator 调用**——`tools.Registry.Definitions()` 按字母排序（KV-cache 友好，见 [07_tools](07_tools.md)）。Policy 提供的是**"如果将来想换种排序"**的能力，目前主要用 `FormatContextHint()` 间接生效。

### 7.3 序列推荐

```go
SuggestNext(lastTool) string
  扫所有 sequences[lastTool→X]
  返回成功率最高的 X (rate > 0.7)
```

`policy.go:232-244` 那段手动解析 UTF-8 "→" 的代码（`0xe2 0x86 0x92`）是 Go map key 用字符串导致——后续如果改成 `struct{From, To string}` 会更清爽，但当前实现可用且零依赖。

### 7.4 上下文提示

`FormatContextHint(lastTool)` 输出形如：

```
[Tool Learning Insights]
- write_file has low reliability (38% success) — consider alternatives
- run_tests is degrading — recent calls fail more often
- After read_file, edit_file typically succeeds
```

每次 ReAct prompt 都会注入这个 hint（前缀稳定的 token，依然 KV cache 友好）。

---

## 8. `Advisor` —— 单工具警告

`Advisor.Advise(toolName)`：

```go
pattern := extractor.GetPattern(toolName)
if pattern == nil || pattern.SampleSize < 5: return nil

if pattern.FailureRate > 0.5:
    advice.Warning = "Tool %q has %.0f%% failure rate, common errors: ..."

if pattern.AvgDuration > 10000:   # 10s
    advice.Hint = "Tool %q averages %dms — consider alternative"
```

阈值是经验值：
- **失败率 > 50%** 算"不靠谱"——低于这个的工具偶尔挂是正常波动；
- **平均耗时 > 10s** 算"慢"——sandbox / MCP / LSP 的多数调用都在 5s 以内。

`FormatForLLM` 把 warning + hint 拼成单条 `[Tool Learning Warning]` 文本。

---

## 9. `PGStore` —— 持久化

```sql
CREATE TABLE tool_feedback (
  id BIGSERIAL PRIMARY KEY,
  tool_name TEXT NOT NULL,
  args_hash TEXT NOT NULL,
  success BOOLEAN NOT NULL,
  duration_ms INT NOT NULL,
  error_msg TEXT DEFAULT '',
  session_id TEXT NOT NULL,
  created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX idx_tool_feedback_tool ON tool_feedback(tool_name);
CREATE INDEX idx_tool_feedback_created ON tool_feedback(created_at);
```

`GetPatterns` 是聚合查询，**只看最近 7 天**——避免老数据冲淡当前 pattern。

**当前未用作启动加载**：进程启动时 Collector 的 buffer 是空的；GetPatterns 主要是给 dashboard 或调试用。这意味着 cold start 后需要几十次工具调用才能形成有效 pattern——这是个改进点。

---

## 10. 与其他模块的边界

### 10.1 上游：orchestrator

```go
// internal/orchestrator/orchestrator.go
type Orchestrator struct {
    toolCollector *toollearn.Collector
    toolAdvisor   *toollearn.Advisor
    toolPolicy    *toollearn.AdaptivePolicy
    toolDistiller *toollearn.Distiller
}

// 构造（内存模式，未启用 PG 持久化）
collector := toollearn.NewCollector(nil, logger)
extractor := toollearn.NewExtractor(collector, logger)
orch.toolAdvisor = toollearn.NewAdvisor(extractor, logger)
orch.toolPolicy = toollearn.NewAdaptivePolicy(collector)
orch.toolDistiller = toollearn.NewDistiller(collector, logger)
```

**注意**：当前 main.go 构造时 `Collector` 用的是 `nil` store——意味着 feedback 只活在进程内存。要启用持久化，需要：

```go
pgStore := toollearn.NewPGStore(orch.db)
pgStore.Migrate()
collector := toollearn.NewCollector(pgStore, logger)
```

### 10.2 平行：agentloop.AdaptiveFeedback

见 §1 对比表。两者**互不通信**，但语义上互补。

### 10.3 下游：Postgres

通过 `database/sql` 标准库的 `*sql.DB`，与 `internal/store` 共享同一个连接池。Schema 自迁移（`PGStore.Migrate()` on startup）。

---

## 11. 设计权衡

| 抉择 | 动机 |
|------|------|
| ArgsHash 替代 Args | 隐私 + 去重；代价是无法反查具体参数 |
| buffer 满了直接丢 | toollearn 是"额外能力"，不能反过来拖累主流程 |
| 4 个产物（pattern/strategy/advice/score） | 不同抽象层级的视角；融合反而失去信号 |
| Distiller 只学 ≥70% 成功的 session | 噪声 session 学到的"经验"会误导 |
| 短链替换长链 | 偏好简洁工具序列；与 ReAct 的"少即是多"一致 |
| 任务 5 分类硬编码 | 软分类（embedding）成本高且不稳定；5 类够用 |
| 阈值经验值（50% / 10s / 5 样本） | 来自实测；启动时低样本不输出 advice 避免误导 |
| EMA 0.8/0.2 平滑 | 避免单次异常翻车（一次失败把 success rate 拉下来太多） |
| 不在启动时回灌 PG → buffer | 简化启动逻辑；cold start 慢但代价低 |
| Policy.RankTools 已实现但未启用 | 改 tools.Registry 排序会破坏 KV cache 命中；保留为可选 |

---

## 12. 后续演进

- [ ] **启动回灌**：进程启动时从 PG 读最近 7 天 feedback 填到 Collector buffer——cold start 即可拥有完整经验
- [ ] **Per-user 学习**：当前所有用户共享一份经验池；区分 user_id 让每个用户的"经验"独立（与 [18_auth](18_auth_security.md) 联动）
- [ ] **embedding 任务分类**：替代 hardcoded 5 类——用 LLM embedding 把 taskHint 映射到学到的 cluster
- [ ] **Streaming distill**：当前 `Distill()` 是 batch；改成 worker goroutine 持续吃 buffer
- [ ] **失败因果分析**：CommonErrors 现在按文本聚合；加入 `agentloop.ClassifyToolError` 的类别可以做"权限错误占 X%、路径错误占 Y%"分布
- [ ] **A/B 测试 advisor**：50% 流量开 advisor、50% 关，对比工具成功率变化，量化 toollearn 的真实增益
- [ ] **Prometheus 指标**：`toollearn_pattern_updates_total{tool}`、`toollearn_strategy_recommended_total{pattern}`
- [ ] **数据治理**：tool_feedback 表无 TTL，长期会膨胀；加 7 天/30 天分区或定期 truncate
- [ ] **Negative example 学习**：当前只学成功；从失败 session 提取"反模式"（先 run_tests 再 read_file 是常见错误流程）

---

## 13. 与人类工程师的类比

工程师入职 1 个月后，会自然知道：
- "这个仓库的 build 系统坏，先 `make clean` 比直接 build 成功率高"
- "review 工具不准，CI 跑出来才是真的"

`toollearn` 让 agent 也能积累这种**"环境特定的偏好"**。它不是 LLM-pretrained 的通用知识，而是**本仓库 / 本 agent / 本 sandbox** 的局部经验——这是任何 SaaS 模型都拿不到、只能本地积累的资产。

正因此，toollearn 的数据**很值钱但很脆**：迁移到新环境后这些经验大半失效。后续可以加入"环境指纹"区分本地经验的适用范围。

---

下一篇：[`24_treesitter.md`](24_treesitter.md) —— Tree-sitter AST 解析：CGO 与正则降级双模式实现。
