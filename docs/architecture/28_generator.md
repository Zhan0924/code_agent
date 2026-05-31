# 28 · 项目生成器 `internal/generator`

> 代码：
> - `generator.go` (605) — 五阶段 pipeline + 蓝图 / 拓扑排序 / 模板 / 校验 / 自修复
>
> 测试：`generator_test.go` (254) — 单元测试覆盖 topoSort / parseBlueprint / generateTemplate

---

## 1. 模块定位

**"输入一句话（'帮我搭一个 Go Gin REST API'），输出一个能 `go build` 通过的可运行项目骨架。"**

`internal/generator` 是 code_agent **整体能力的展示窗口**——把所有零散子系统（LLM / sandbox / workspace）按一个固定的"项目工程化"流程编排起来，给用户的是一个**完成度可见的**端到端产品。

它**不是** ReAct 循环。ReAct 适合"探索性"任务（"修个 bug"），但项目生成是**结构化** + **大粒度**的任务（"从零搭起 25 个文件"）——用固定的五阶段 pipeline 比让 LLM 自己开 ReAct 决定下一步要做什么更靠谱：

- ReAct 容易陷入 "我先写 main.go → 写完发现需要 model → 改回去补 → ... → 8 步后 token 超限"；
- pipeline 把"先想结构，再按依赖顺序填代码"这种**人类工程师工作方式**硬编码进流程。

### 五阶段

```
Phase 1 Blueprint   →  LLM 产 JSON 描述整个项目结构
Phase 2 Scaffold    →  无 LLM，按蓝图建目录 + 写死模板文件
Phase 3 Implement   →  按拓扑序，每个文件一次 LLM 调用
Phase 4 Validate    →  sandbox 跑 build；失败时 LLM 自修复一轮
Phase 5 Polish      →  生成 README.md
```

每阶段都有**可观察的产出**（蓝图、脚手架、源码、构建结果、文档）——这是 5 阶段而不是 1 个大 prompt 的核心理由。

---

## 1.5 核心设计问题

### 为什么不一次性让 LLM 生成全部文件？

试过的人都知道，"一次给我整个项目"会出现：

1. **Token 上限**：30 个文件的 Go 项目轻松 2-5 万 token，超 LLM 单 response 限制；
2. **后文忘前文**：写到第 20 个文件时 LLM 已经忘记前面 main.go 用了什么 package；
3. **没法 stream**：用户等 60 秒看不到任何东西，感觉死机；
4. **错误传染**：第 3 个文件类型签名错了，后续 27 个文件全部基于错误假设。

**Per-file LLM call** 解决全部 4 个问题：
- 每次调用上下文小（蓝图 + 当前文件描述 + 依赖文件内容 ≈ 4K token）；
- 拓扑序保证 model.go 先于 user_service.go 生成，依赖关系靠 `buildDependencyContext` 显式传；
- SSE 让前端每生成一个文件就闪一行进度；
- 每 5 个文件跑一次 sandbox 校验，错误早发现早修。

代价是 LLM 调用次数 = 文件数 + 1（蓝图）+ 校验时的修复轮数。30 个文件 × ~2K token/call ≈ 60K token 总消耗——比一次性生成省得多（一次性生成需要把 30 个文件的完整内容塞进单 response，model 必须保留全部上下文）。

### 为什么用 LLM 产 JSON 蓝图而不是预定义模板？

模板方案（"Go REST API 模板""Python FastAPI 模板"）的问题：

- 用户需求是开放的（"REST API + 内置 Prometheus metrics + JWT auth + Postgres + Redis 缓存"——哪个固定模板能满足？）；
- 模板维护成本高（语言 × 框架 × 数据库 × 中间件的笛卡尔积）；
- 模板里 30 个文件中大多数文件 **80% 内容仍然要根据业务描述变**，模板只能省少数纯样板（go.mod / Dockerfile / Makefile）。

**LLM 设计 + 人工预制模板**：
- LLM 产蓝图列出"哪 30 个文件"（这是**结构性**决策，LLM 擅长）；
- 已知样板（go.mod / .gitignore / Dockerfile / Makefile）走 `generateTemplate` 无 LLM 调用——省 token 又稳定；
- 业务文件（main.go / handler.go / model.go）才让 LLM 真写。

这是"**LLM 做创造性，代码做确定性**"的典型分工。

### 为什么校验放在生成途中而不是最后？

`for i := range files { ... if status.FilesGenerated%5 == 0: validateAndFix(...) }` —— 每 5 个文件触发一次 sandbox build。

为什么不一次性生成完再校验？

- 30 个文件生成完才发现第 3 个文件类型错了，要求 LLM "修第 3 个文件然后审视 4-30 是否需要跟着改"——给 LLM 加了**指数级**的认知负担；
- 早校验早纠错：每 5 个文件校验时，LLM 修起来只需要看局部错误 + 一两个最近文件。

代价：早校验在 build incomplete 时会报很多"unresolved symbol"假警报——`attemptAutoFix` 必须能识别"这是因为还没生成完，不是真错"。当前实现是**只修被错误明确指名的文件**，不指名的文件不动——隐式过滤掉了"unresolved future symbol"误报。

### 为什么 file 生成失败不 abort 全流程？

```go
content, err := g.generateFile(...)
if err != nil {
    g.logger.Warn("file generation failed, continuing", ...)
    status.Errors = append(status.Errors, ...)
    continue   // ← 不 return
}
```

**部分成功 > 全失败**：30 个文件里第 17 个 LLM 调用偶发超时，剩下 13 个文件其实独立可生成。让用户拿到 "29/30 文件 + 一条 errors" 比直接告诉他 "失败重试吧" 友好得多。

当然这有代价：错误文件留空可能让 build 全黑（缺关键 import）。但**用户能看到具体哪个文件没生成 + 错误信息**，可以手动修或重新触发——这比"生成失败"四个字提供的信息多得多。

---

## 2. 依赖架构

```
                ┌────────────────────────────────────────┐
                │  POST /api/v1/projects/generate/stream │
                │  {"description": "Gin REST API ..."}   │
                └────────────────┬───────────────────────┘
                                 │
                                 ▼
                ┌────────────────────────────────────────┐
                │  Server.handleGenerateProjectSSE       │
                │  ├─ SSE Headers                        │
                │  ├─ flusher = c.Writer.(http.Flusher)  │
                │  ├─ onProgress = func(evt) {           │
                │  │     fprintf "data: %s\n\n"          │
                │  │     flusher.Flush()                 │
                │  │   }                                 │
                │  └─ generator.Generate(ctx, desc, ...) │
                └────────────────┬───────────────────────┘
                                 │
                                 ▼
                ┌────────────────────────────────────────┐
                │  Generator.Generate (sync, 5 phases)   │
                └─┬──────────────┬─────────┬───────────┬─┘
                  │              │         │           │
        ┌─────────┘              │         │           └─────────┐
        ▼                        ▼         ▼                     ▼
   ┌──────────┐         ┌────────────┐  ┌──────────┐    ┌────────────┐
   │ llm.     │         │ workspace. │  │ sandbox. │    │ workspace. │
   │ Client   │         │ Manager    │  │ Manager  │    │ Manager    │
   │ (4 调用) │         │ Create/    │  │ Execute  │    │ ReadFile / │
   │ blueprint│         │ MkdirAll/  │  │ WithVol  │    │ TreeString │
   │ +file*N  │         │ WriteFile/ │  │ (build   │    │ (docs)     │
   │ +fix     │         │ ReadFile   │  │ check)   │    │            │
   │ +docs    │         │            │  │          │    │            │
   └──────────┘         └────────────┘  └──────────┘    └────────────┘
                                 │
                                 ▼
                ┌────────────────────────────────────────┐
                │  Per-project state                     │
                │  generator.projects[id] = ProjectStatus│
                │  Workspace at /tmp/agent-workspaces/id │
                └────────────────────────────────────────┘
```

---

## 2.5 数据流总览

```text
═════════════ Generate("帮我搭一个 Go Gin REST API") 全流程 ═════════════

[Input] description = "帮我搭一个 Go Gin REST API，带 JWT auth 和 Postgres"

╔═ Phase 1: Blueprint ═══════════════════════════════════════════════╗
║ emit("blueprint", "Generating project blueprint...")               ║
║                                                                    ║
║ prompt = blueprintPrompt(description)                              ║
║         "You are a senior software architect..."                   ║
║         "Generate 10-30 files for a realistic structure..."        ║
║         "Respond with ONLY a valid JSON object..."                 ║
║                                                                    ║
║ resp := llm.Complete(prompt)  ← 1 次 LLM 调用                       ║
║ strip ```json fences                                               ║
║ json.Unmarshal → ProjectBlueprint{Name, Language, Files[]}         ║
║                                                                    ║
║ topoSort(blueprint)                                                ║
║   Kahn's algorithm: 按 Dependencies 计算 Priority                  ║
║   files = [go.mod(P0), config.go(P0), model.go(P0),               ║
║            user_repo.go(P1), user_service.go(P2),                  ║
║            handler.go(P3), main.go(P4)]                            ║
╠══════════════════════════════════════════════════════════════════════╣

╔═ Phase 2: Scaffold (no LLM) ════════════════════════════════════════╗
║ ws := workspaceMgr.Create(projectID, blueprint.Name)               ║
║                                                                    ║
║ dirs := {"cmd/server", "internal/api", "internal/models", ...}     ║
║ MkdirAll(dir for dir in dirs)                                      ║
║                                                                    ║
║ for f in files where f.Type in ["template","config"]:              ║
║   content := generateTemplate(bp, f)   ← 写死的模板                ║
║   if content != "":                                                ║
║     WriteFile(f.Path, content)                                     ║
║     f.Generated = true                                             ║
║                                                                    ║
║ → go.mod / .gitignore / Dockerfile / Makefile 直接落盘             ║
╠══════════════════════════════════════════════════════════════════════╣

╔═ Phase 3: Implementation (N 次 LLM) ════════════════════════════════╗
║ for i, f in files:                                                 ║
║   if f.Generated: continue   # scaffold 阶段已生成                 ║
║                                                                    ║
║   emit("implement", "Generating " + f.Path)                        ║
║                                                                    ║
║   depCtx := buildDependencyContext(bp, f, ws)                      ║
║     for dep in f.Dependencies:                                     ║
║       content = ReadFile(dep)         # 已生成的依赖文件           ║
║       if len > 3000: truncate         # 控 token                   ║
║       sb += "--- " + dep + " ---\n" + content                      ║
║                                                                    ║
║   prompt = fileGenPrompt(language, path, project, depCtx)          ║
║   resp := llm.Complete(prompt)         ← 1 次 LLM 调用              ║
║   strip ``` fences                                                 ║
║   WriteFile(f.Path, content)                                       ║
║   f.Generated = true                                               ║
║   FilesGenerated++                                                 ║
║                                                                    ║
║   if FilesGenerated % 5 == 0:                                      ║
║     validateAndFix(...)             # 见 Phase 4                   ║
╠══════════════════════════════════════════════════════════════════════╣

╔═ Phase 4: Validation (sandbox + 可选自修复) ════════════════════════╗
║ buildCmd := "cd /workspace && go build ./... 2>&1"                 ║
║ result := sandboxMgr.ExecuteWithVolume("go", buildCmd, ws.RootDir) ║
║                                                                    ║
║ if result.ExitCode != 0:                                           ║
║   errors = result.Stdout[:300]                                     ║
║   status.Errors += errors                                          ║
║                                                                    ║
║   # 自修复一轮                                                      ║
║   prompt = "build errors:\n" + errors + "\nRespond with JSON[]"    ║
║   resp := llm.Complete(prompt)                                      ║
║   fixes := json.Unmarshal(resp)        ← [{path, content}, ...]    ║
║   for fix in fixes: WriteFile(fix.Path, fix.Content)               ║
║                                                                    ║
║   # 不再二次校验——一轮就停                                          ║
╠══════════════════════════════════════════════════════════════════════╣

╔═ Phase 5: Polish ═══════════════════════════════════════════════════╗
║ if README.md 已存在: skip                                          ║
║                                                                    ║
║ tree := workspaceMgr.TreeString(ws)                                ║
║ prompt = "Generate README.md for project: " + tree                 ║
║ resp := llm.Complete(prompt)             ← 1 次 LLM 调用            ║
║ WriteFile("README.md", resp.Content)                               ║
╠══════════════════════════════════════════════════════════════════════╣

[Output] ProjectStatus{
  Phase: "done",
  FilesGenerated: 28,         # 比预期少 2（中间有失败）
  Errors: ["user_service.go: timeout"],
  WorkspaceID: "ws-xxx",      # 用户可在 /workspace 浏览
  Duration: 4m23s,
}
```

---

## 3. 数据结构

### 3.1 `ProjectBlueprint`

```go
type ProjectBlueprint struct {
    Name        string     // "user-service"
    Language    string     // "go" / "python" / "typescript"
    Framework   string     // "gin" / "fastapi" / "express"
    Description string     // 一句话
    Files       []FileSpec
}

type FileSpec struct {
    Path         string   // "cmd/server/main.go"
    Type         string   // "code" / "test" / "config" / "doc" / "template"
    Description  string   // "Main entry point with HTTP server setup"
    Dependencies []string // 其他 file paths
    Priority     int      // 拓扑排序后填，0=最先
    Generated    bool     // 生成完成标记
}
```

**关键设计**：
- `Path` 用 `/` 分隔（跨 OS 一致），`workspace.Manager` 在写文件时做平台转换；
- `Type=template` 是触发"无 LLM 生成"的标记——`go.mod` / `.gitignore` 等走这条路径；
- `Dependencies` 是**文件级**依赖（"main.go 依赖 handler.go"），不是 import 级——粗粒度但够 LLM 拓扑排序用。

### 3.2 `ProjectStatus`

```go
type ProjectStatus struct {
    ID             string
    Phase          string             // "blueprint" / "scaffold" / "implement" / "validate" / "polish" / "done" / "failed"
    Blueprint      *ProjectBlueprint  // Phase 1 后填充
    FilesTotal     int
    FilesGenerated int
    Errors         []string
    WorkspaceID    string             // /tmp/agent-workspaces/<id>
    StartedAt      time.Time
    CompletedAt    *time.Time
}
```

`Phase` 字符串是**前端的进度条状态依据**——SSE 里每个 event 都带 phase，前端用它切换 UI 状态。

### 3.3 `ProgressEvent`

```go
type ProgressEvent struct {
    Phase   string   // 同上
    Message string   // 人读："Generating cmd/server/main.go..."
    File    string   // 可选：当前正在处理的文件
    Done    int      // 已完成数
    Total   int      // 总数
}
```

SSE 通过 `data: <json>\n\n` 推这个对象——前端可以画 "23/30" 的进度条 + 滚动日志。

---

## 4. Phase 1: Blueprint —— JSON 蓝图生成

### 4.1 Prompt 设计

```text
You are a senior software architect. Generate a project blueprint as a JSON object.

User requirement: %s

Respond with ONLY a valid JSON object (no markdown fences) with this exact structure:
{
  "name": "...", "language": "...", "framework": "...",
  "files": [{"path": "...", "type": "...", "description": "...", "dependencies": [...]}]
}

Rules:
- Generate 10-30 files for a realistic project structure
- Include: source code, tests, config files, Dockerfile, Makefile, README.md, go.mod
- For Go projects: follow standard layout (cmd/, internal/, pkg/, configs/, deployments/)
- "dependencies" lists OTHER file paths that this file imports/depends on
- Type is one of: "code", "test", "config", "doc", "template"
```

**注意点**：
- **"ONLY a valid JSON object (no markdown fences)"** —— LLM 经常无视这条还是带 ```json fence；代码在解析前必须 strip（见 §4.2）；
- **"10-30 files"** 上限：超 30 个文件会让总 token 消耗失控；少于 10 个又显得太简陋；
- **"for Go projects: follow standard layout"** —— 显式 anchor 到 Go 社区惯例（`cmd/`, `internal/`），避免 LLM 自由发挥出诡异结构；
- **Type 枚举** —— Phase 2 用 type 决定是否走模板；枚举写死避免 LLM 创造 "doc" / "documentation" / "docs" 多种写法。

### 4.2 JSON 解析容错

```go
content := strings.TrimSpace(resp.Content)
content = strings.TrimPrefix(content, "```json")
content = strings.TrimPrefix(content, "```")
content = strings.TrimSuffix(content, "```")
content = strings.TrimSpace(content)

var blueprint ProjectBlueprint
if err := json.Unmarshal([]byte(content), &blueprint); err != nil {
    return nil, fmt.Errorf("failed to parse blueprint JSON: %w\nRaw response: %s",
        err, content[:min(len(content), 500)])
}
```

容错策略：
1. **三次 strip**：`json` 前缀 + `` ``` `` 前缀 + `` ``` `` 后缀（顺序敏感）；
2. **失败时把 raw 前 500 字符塞进 error message** —— debug 时能直接看到"LLM 实际返回了什么"；
3. **`len(blueprint.Files) == 0` 也算错** —— 防御性检查，LLM 可能返回合法 JSON 但 files 数组为空。

**未实现**：JSON repair（LLM 返回截断的 JSON 时用 `jsonrepair` 库补全 `}`）。当前一次失败就整体 fail——后续可加 retry + repair。

### 4.3 拓扑排序 (Kahn's Algorithm)

```go
func topoSort(bp *ProjectBlueprint) {
    pathIdx := make(map[string]int)
    for i, f := range bp.Files {
        pathIdx[f.Path] = i
    }
    
    inDegree := make(map[int]int)
    adj := make(map[int][]int)
    for i, f := range bp.Files {
        for _, dep := range f.Dependencies {
            if j, ok := pathIdx[dep]; ok {
                adj[j] = append(adj[j], i)   // dep → me
                inDegree[i]++
            }
        }
    }
    
    queue := []int{} // 入度为 0 的节点
    for i := range bp.Files {
        if inDegree[i] == 0 { queue = append(queue, i) }
    }
    
    priority := 0
    for len(queue) > 0 {
        next := []int{}
        for _, idx := range queue {
            bp.Files[idx].Priority = priority
            for _, neighbor := range adj[idx] {
                inDegree[neighbor]--
                if inDegree[neighbor] == 0 { next = append(next, neighbor) }
            }
        }
        queue = next
        priority++
    }
    
    sort.SliceStable(bp.Files, func(i, j int) bool {
        return bp.Files[i].Priority < bp.Files[j].Priority
    })
}
```

**为什么用 BFS 层序而不是 DFS 后序？**
- BFS 给同层节点**同一个 Priority**——同层 1 个文件失败不影响同层其他文件（"main.go 失败不影响同层的 config.go"）；
- DFS 给每个文件**唯一序号**——失败传染更明显（17 号失败让 LLM 误以为 18 号也要等）；
- BFS 还便于将来**并发**生成同层文件（当前是串行，未来可并发）。

**循环依赖怎么办？**
- Kahn's 跑完后入度 > 0 的节点表示有环——当前实现**默认无环**，环里的节点 priority 不会被赋值（保持 0），sort 后会出现在最前面，可能导致生成顺序错。
- 这是**已知缺陷**：blueprint prompt 不强制 acyclic，LLM 偶尔会写 "a.go 依赖 b.go 且 b.go 依赖 a.go"。补完时可在 topoSort 末尾检查残留入度，>0 报错。

### 4.4 `Priority` 排序后是稳定的吗？

`sort.SliceStable`——同 Priority 的文件**保持原序**。这是有意为之：LLM 在 blueprint 里把 main.go 放最后是它的"叙事直觉"（先讲依赖再讲使用方），代码尊重这个顺序能让"同层"内的生成顺序更直觉。

---

## 5. Phase 2: Scaffold —— 目录 + 模板

### 5.1 目录创建

```go
dirs := make(map[string]bool)
for _, f := range bp.Files {
    dir := strings.TrimSuffix(f.Path, "/"+lastSegment(f.Path))
    if dir != f.Path && dir != "" {
        dirs[dir] = true
    }
}
for dir := range dirs {
    g.workspaceMgr.MkdirAll(ws, dir)
}
```

**关键细节**：
- `dir != f.Path` 排除根级文件（如 `go.mod` 的 dir 计算结果是 `""`）；
- `MkdirAll` 是 `mkdir -p` 等价，安全地建多级目录；
- 用 map 去重——5 个文件同在 `internal/api/` 下只调 `MkdirAll` 一次。

### 5.2 `generateTemplate` 模板

```go
switch {
case name == "go.mod" && bp.Language == "go":
    return "module github.com/example/<name>\n\ngo 1.23\n"
case name == ".gitignore":
    return "# Build\nbin/\n*.exe\n*.o\n\n# IDE\n.idea/\n.vscode/\n*.swp\n..."
case name == "Dockerfile" && bp.Language == "go":
    return "FROM golang:1.23-alpine AS builder\n..."
case name == "Makefile" && bp.Language == "go":
    return ".PHONY: build test lint run\nbuild:\n\tgo build...\n"
}
return ""
```

**模板覆盖**：当前只覆盖 Go 项目的 4 个文件（go.mod / .gitignore / Dockerfile / Makefile）。
- Python 项目对应的 `pyproject.toml` / `Dockerfile` / `Makefile` **未模板化**——会走 LLM 路径生成；
- TypeScript 项目同理。

**为什么不全语言覆盖？** Go 的样板高度同质（90% Go 项目的 go.mod 三行），Python / TS 的样板差异巨大（dependencies、build tools 各异），LLM 生成 vs 硬编码模板的胜率不明显——干脆只保留确定收益最高的部分。

**`generateTemplate` 返回 `""` 不报错** —— 表示"我对这个文件没意见，去走 LLM 吧"。这个"空返回 = 跳过模板"的约定让模板列表可以无痛扩展。

---

## 6. Phase 3: Implementation —— per-file LLM

### 6.1 Prompt 结构

```text
You are a senior <lang> developer. Generate the file: <path>

Project: <name> (<lang> using <framework>)
Purpose of this file: <description>

<depCtx>

Rules:
- Produce ONLY the file content, no markdown fences, no explanations
- Follow idiomatic <lang> conventions and best practices
- Include proper error handling
- Add doc comments for all exported symbols
- If this is a test file, use table-driven tests
```

**Per-file 的 prompt 大小**：
- 固定部分约 250 token；
- `depCtx` 视依赖数变化，单文件依赖 3000 字符截断，5 个依赖 → ~3K token；
- 总 prompt ≈ 4K token，response ≈ 1-3K token。

**"no markdown fences"** 这条 LLM 经常违反——`generateFile` 的 strip 逻辑兼容：

```go
if strings.HasPrefix(content, "```") {
    lines := strings.SplitN(content, "\n", 2)
    if len(lines) > 1 { content = lines[1] }   // 去开头 ```lang
    if idx := strings.LastIndex(content, "```"); idx > 0 {
        content = content[:idx]                  // 去结尾 ```
    }
}
```

**注意**：这里**不**像 blueprint 一样只 strip 三种固定前缀，而是处理"开头一行是 fence + 末尾任意位置出现 fence"——更宽容，因为 LLM 给代码加 fence 的概率高于给 JSON 加 fence。

### 6.2 `buildDependencyContext`

```go
func (g *Generator) buildDependencyContext(bp, f, ws) string {
    if len(f.Dependencies) == 0 { return "" }
    
    sb := "Related files already generated:\n\n"
    for _, dep := range f.Dependencies {
        content, err := g.workspaceMgr.ReadFile(ws, dep)
        if err != nil { continue }      // dep 还没生成或读不到
        if len(content) > 3000 {
            content = content[:3000] + "\n// ... truncated ..."
        }
        sb += "--- " + dep + " ---\n" + content + "\n\n"
    }
    return sb
}
```

**为什么是 3000 字符截断而不是 token 计数？**
- 字符截断快、零依赖；
- 3000 字符 ≈ 1000 token（英文）或 600-800 token（代码 + 中文注释）；
- 一个文件被截断到 3000 字符通常包含完整的 import + struct 定义 + 主要函数签名——对 LLM 写"调用我"的下游文件足够。

**截断的风险**：
- 依赖文件的 helper 函数被砍掉，LLM 写下游时可能漏调；
- 截断标记 `// ... truncated ...` 是 Go 语法的注释，对 LLM 是显式信号"这里有内容你看不到，谨慎引用"；
- Python / TS 文件用 `// ...` 是语法错——**已知 bug**。补完时按语言切换注释格式。

### 6.3 失败容错

```go
content, err := g.generateFile(ctx, blueprint, f, ws)
if err != nil {
    g.logger.Warn("file generation failed, continuing", ...)
    status.Errors = append(status.Errors, fmt.Sprintf("%s: %v", f.Path, err))
    continue   // ← 不 abort
}

if err := g.workspaceMgr.WriteFile(ws, f.Path, content); err != nil {
    status.Errors = append(status.Errors, ...)
    continue
}
```

**两层容错**：LLM 失败（超时 / 429）和写盘失败（磁盘满 / 权限）都 continue。错误累积到 `status.Errors`，最终 status.Phase 仍是 "done"——是否成功由调用方根据 `len(Errors)` 判断。

---

## 7. Phase 4: Validation —— sandbox 校验 + 自修复

### 7.1 触发时机

```go
// 每 5 个文件
if status.FilesGenerated > 0 && status.FilesGenerated%5 == 0 {
    g.validateAndFix(ctx, blueprint, ws, status)
}

// 阶段结束
emit("validate", ...)
g.validateAndFix(ctx, blueprint, ws, status)
```

中间每 5 个一校验 + 最后再校验一次。频率选 5 是经验值：
- 太频繁（每 1 个）：sandbox 启动开销（1-2s）累积可观；
- 太稀疏（每 20 个）：错误传染范围大，修起来更难。

### 7.2 多语言 build 命令

```go
switch bp.Language {
case "go":
    buildCmd = "cd /workspace && go build ./... 2>&1"
case "python":
    buildCmd = "cd /workspace && python3 -m py_compile *.py 2>&1"
case "typescript", "javascript":
    buildCmd = "cd /workspace && npx tsc --noEmit 2>&1"
default:
    return                           // 不支持的语言跳过校验
}
```

**为什么 Python 用 `py_compile *.py`**？纯 Python 没有 build 概念，`py_compile` 是语法检查（不执行）。代价：只检查根目录文件，子目录的 .py 不会被检查（应该是 `find . -name "*.py" -exec python3 -m py_compile {} \;` 但当前简化处理）。

**`2>&1`** 把 stderr 并到 stdout——result.Stdout 拿到全部输出。Go / TS 的编译错误都在 stderr。

### 7.3 `ExecuteWithVolume` 与普通 sandbox 的区别

```go
result, err := g.sandboxMgr.ExecuteWithVolume(ctx, lang, buildCmd, ws.RootDir)
```

`ExecuteWithVolume` vs `Execute`（[05_sandbox](05_sandbox.md)）：
- 普通 `Execute` 把源码作为字符串传入容器临时文件；
- `ExecuteWithVolume` 把 `ws.RootDir` (`/tmp/agent-workspaces/<id>`) **挂载**进容器 `/workspace`，整个项目可见——`go build ./...` 才能跨文件 import 解析。

`NetworkMode: none` 仍然生效——容器看不到外部网络，但能访问 workspace 文件。这是 generator 唯一**需要 volume mount** 的子系统（其他工具都是单文件传入）。

### 7.4 自修复一轮策略

```go
prompt := "The %s project has build errors:\n%s\n\n
          Respond with a JSON array of fixes:
          [{"path": "...", "content": "full file"}]
          Only include files that need changes."

resp := llm.Complete(prompt)
strip fences
json.Unmarshal → []fix

for fix in fixes: WriteFile(fix.Path, fix.Content)
```

**关键约束**：
- **一轮**（不二次校验）——避免无限循环修不好就跑死 LLM 配额；
- **"Only include files that need changes"** —— prompt 显式要求 LLM 不要返回 30 个文件（防 token 爆炸 + 防"修一个错误顺手改 5 个无关文件"）；
- `errors[:2000]` 截断错误输出——build 错误日志可能有几万字符（Go cyclic import 的 trace），LLM 看前 2000 字符够定位主要问题。

**为什么不二次校验？**
- 修了之后再 build → 还失败 → 再修 → ... → 容易死循环；
- 自修复**是辅助**——失败时让用户拿到"+ 1 轮修复尝试"而不是"放弃"；
- 真正的工程化方式是**重新触发 Generate**（用户看到 errors 后决定）。

---

## 8. Phase 5: Polish —— README

```go
func (g *Generator) generateDocs(ctx, bp, ws) {
    if _, err := g.workspaceMgr.ReadFile(ws, "README.md"); err == nil {
        return   // blueprint 已包含 README.md 走 Phase 3 生成过，跳过
    }
    
    tree := g.workspaceMgr.TreeString(ws)
    prompt := "Generate README.md ... Directory structure:\n" + tree
    resp := llm.Complete(prompt)
    g.workspaceMgr.WriteFile(ws, "README.md", resp.Content)
}
```

**为什么 README 单独放在 Phase 5？**
- README 要"看到全部文件之后再写"——结构图 / 主要功能描述都依赖完整代码；
- 如果 blueprint 把 README.md 列进 Files 数组，Phase 3 会**按拓扑序**生成它——但 README 的 dependencies 应该是**所有文件**，Phase 3 拓扑序会把它排到最末，效果其实相同。
- Phase 5 还是单独走，是因为**即使 blueprint 没列 README.md** 也保证生成——用户拿到的项目永远有 README。

### 8.1 `TreeString`

`workspace.Manager.TreeString(ws)` 返回 `tree` 命令样式的目录结构：

```
.
├── cmd/server/
│   └── main.go
├── internal/
│   ├── api/
│   │   └── handler.go
│   └── models/
│       └── user.go
└── go.mod
```

这是给 LLM 看的"结构鸟瞰"——LLM 据此写"## Project Structure"章节。

---

## 9. API 暴露

### 9.1 三个端点

```
POST /api/v1/projects/generate         -- 异步 fire-and-forget
POST /api/v1/projects/generate/stream  -- SSE 实时进度
GET  /api/v1/projects/:id/status       -- 轮询拿状态
```

`/generate` 走 background goroutine + 30 分钟超时：

```go
go func() {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
    defer cancel()
    status, err := s.generator.Generate(ctx, req.Description, nil)
    // status.ID 已被 Generator.Generate 在入口写入 g.projects[id]——
    // 通过 generator.GetStatus(id) 仍可查；但 handler 没把 id 透到 HTTP 响应
}()
c.JSON(http.StatusAccepted, gin.H{
    "status":  "generation_started",
    "message": "... use the SSE endpoint for real-time progress.",
})
```

**注意 1（生命周期）**：goroutine **不**关联 HTTP request context——client 断开不会取消生成。这是有意的（让"提交后关浏览器"也能完成），但代价是用户没法通过 abort HTTP 请求来 cancel 生成（必须等 30 分钟超时或 server 重启）。

**注意 2（已知缺陷：响应里没有 project_id）**：

`Generator.Generate` 在入口就做了：
```go
projectID := uuid.New().String()
status := &ProjectStatus{ID: projectID, Phase: "blueprint", ...}
g.mu.Lock()
g.projects[projectID] = status                  // ← 写入内存状态表
g.mu.Unlock()
```
所以**状态存储是健全的**——`Generator.GetStatus(projectID)` 可以正常查到任意正在跑的项目。

**真正的问题在 HTTP handler**：`handleGenerateProject` 只返回 `{"status":"generation_started", "message":"..."}`，**没有把 `projectID` 透到响应里**。这意味着：

- `/api/v1/projects/:id/status` 路由在后端工作正常，但客户端**不知道要查哪个 ID**；
- 想要实时观察进度只能改走 `/generate/stream`（SSE）；
- 多个生成任务并发时，调用方无法把任务和结果对上号。

修复路径：handler 入口可以 `id := uuid.New().String()` 自己 mint 一个 + 通过额外参数传给 `Generator.Generate` 用作种子；或者更简单——`Generator.Generate` 加同步 prepare 阶段返回 ID（synchronously create entry, then async run），handler 立刻 `c.JSON(202, gin.H{"id": status.ID, "status": "pending"})`。已列入演进项（§12）。

### 9.2 SSE 流式

```go
c.Header("Content-Type", "text/event-stream")
flusher, ok := c.Writer.(http.Flusher)

onProgress := func(evt ProgressEvent) {
    data, _ := json.Marshal(evt)
    fmt.Fprintf(c.Writer, "data: %s\n\n", data)
    flusher.Flush()              // ← 立即推送
}

status, err := s.generator.Generate(ctx, req.Description, onProgress)
```

`flusher.Flush()` 关键——不调用的话 gin 会缓冲到 8KB 才刷出去，前端**几十秒都看不到任何事件**。这与 [17_api](17_api.md) 的 `/chat/react-stream` 是同一个模式。

前端 Vite 代理（`code_agent_ui/vite.config.ts`）对带 `/stream` 路径的请求**禁用 buffering**——见 CLAUDE.md 提到的代理 SSE 处理。

---

## 10. 与其他模块的边界

| 上游 | 用法 |
|------|------|
| `api.Server` | 三个 HTTP 端点；SSE 适配 `onProgress` 回调 |
| `cmd/agent/main.go:580` | `gen := generator.NewGenerator(...)` + `apiServer.SetGenerator(gen)` |

| 下游 | 用法 |
|------|------|
| [03_llm](03_llm.md) | 4 类调用：blueprint / per-file / fix / docs |
| [05_sandbox](05_sandbox.md) | `ExecuteWithVolume` 跑 `go build` / `tsc --noEmit` |
| [14_workspace](14_workspace.md) | `Create` / `MkdirAll` / `WriteFile` / `ReadFile` / `TreeString` |

**未用到的模块**（虽然 generator 处理项目，但这些不在其依赖里）：
- ❌ `rag` —— generator 不做检索，纯生成；
- ❌ `tools` —— generator 不走 ReAct，不需要工具注册；
- ❌ `memory` —— 跨项目记忆当前未集成（潜在演进：学到"用户偏好 Gin over Echo"）；
- ❌ `multiagent` —— 单进程串行，无并发 sub-agent；
- ❌ `tree-sitter` / `lsp` —— 生成的是新代码，无 AST 解析需求。

---

## 11. 设计权衡

| 抉择 | 动机 |
|------|------|
| 5 阶段固定 pipeline | 比 ReAct 更稳定，结构化任务的合适抽象 |
| LLM 蓝图 + 模板兜底 | 创造性 vs 确定性的分工；省 token |
| Per-file 而非整体 LLM | 单次调用上下文小、可流式、错误隔离 |
| Kahn 拓扑序 BFS | 同层并发友好；DFS 后序无此性质 |
| `sort.SliceStable` | 保留 LLM 蓝图的"叙事顺序"作为同层 tiebreak |
| 每 5 文件校验一次 | 平衡早纠错与 sandbox 启动开销 |
| 自修复**一轮**不二次校验 | 防死循环；多轮修复属于"重新触发 Generate"的范畴 |
| 单文件失败 continue | 部分成功 > 全失败；errors 列表透明告知用户 |
| 30 分钟硬超时 | 防 LLM 卡死无限消耗；30 min 覆盖 30 个文件 × 60s 的中位时长 |
| README.md 单独 Phase 5 | 需要看到完整结构后再写；Phase 3 拓扑序虽能解但 Phase 5 显式 |
| HTTP request 断开不 cancel goroutine | 让"提交后关浏览器"也能完成；代价是无法人为 abort |
| `ExecuteWithVolume` 而非临时文件 | go build ./... 需要跨文件 import 解析 |
| `json.Unmarshal` 失败直接整体 fail | 当前未做 retry / repair；蓝图错了后面都白干 |
| Python / TS 不模板化 | 样板差异大，LLM 生成胜率不显著高于模板 |

---

## 12. 后续演进

- [ ] **JSON repair**：blueprint 截断时用 `jsonrepair` 库补全 `}`，避免一次失败整体 fail
- [ ] **Blueprint retry**：JSON 解析失败时 retry 一次（带原 raw 作为 LLM 反馈："你上次返回的不是合法 JSON，请重新生成"）
- [ ] **同层并发生成**：Phase 3 同 Priority 的文件可并发调用 LLM，10 文件并发 vs 串行可省 80% 时间
- [ ] **二次自修复 + 终止判定**：第一轮修复后再 build，如果 error 数减少就继续修，stagnant 就停
- [ ] **跨语言模板**：Python `pyproject.toml` / TS `package.json` / Rust `Cargo.toml` 等模板化
- [ ] **依赖文件按语言切注释格式**：truncate 标记 `// ... truncated ...` 在 Python 是语法错，改成 `# ... truncated ...`
- [ ] **取消支持**：把 goroutine 的 ctx 关联到 HTTP request ctx + 添加 `DELETE /projects/:id`，client abort 时停止 LLM 调用
- [ ] **断点续生成**：把 ProjectStatus 写 PG，server 重启后能恢复未完成 project
- [ ] **多轮迭代**：在生成完成后允许用户说"再加个 metrics endpoint"——增量修改而不是重新生成
- [ ] **Memory 集成**：[25_memory](25_memory.md) 记录用户偏好（Gin vs Echo / Postgres vs MySQL），蓝图阶段注入
- [ ] **质量评分**：生成完跑 lint + complexity / security scan，给项目打分（"build pass + lint pass + 0 高危漏洞"）
- [ ] **更广的语言支持**：Rust / Java / Kotlin 蓝图 + 校验
- [ ] **可观察性**：`generator_phase_duration_seconds{phase,language}` / `generator_files_generated_total` / `generator_fix_attempts_total`

---

## 13. 与人类工程师"搭新项目"的类比

| 人类 | 本模块 |
|------|--------|
| 在白板上画系统结构图 | Phase 1 Blueprint JSON |
| `git init` + 建目录 + `touch *.go` | Phase 2 Scaffold |
| 一个个文件写代码 | Phase 3 Implementation |
| 写完一批跑 `go build` | Phase 4 Validation (每 5 文件) |
| 看到 errors 修 | Phase 4 attemptAutoFix |
| 最后写 README | Phase 5 Polish |
| 整个过程 30 分钟到 2 小时 | 通常 3-5 分钟 |

agent 比人类快 10×，但代价是：
- 人类可以"边写边调整结构"（写到一半发现需要新增 service.go）；当前 generator **不能**——结构在 Phase 1 锁定；
- 人类的 README 写得更准确（因为知道"我设计的时候是怎么想的"）；agent 的 README 是从 tree + 文件名反推；
- 人类对一个项目的"风格一致性"把控更好（同一个人写的代码风格统一）；agent 每个文件独立 LLM 调用，风格可能微妙差异。

→ generator 适合"快速 spike / 原型"，不适合"长期维护的核心仓库"。这个定位决定了未来演进方向（不是"做更长的项目"，而是"做更稳的小项目"）。

---

下一篇：架构文档全部完成，下面进入 [00_overview](00_overview.md) 更新与 25 篇既有文档的统一优化阶段。
