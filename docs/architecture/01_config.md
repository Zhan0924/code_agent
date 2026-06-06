# 01 · 配置模块 `internal/config`

> 代码：
> - `config.go` (395) — `Config` 根类型 + 18 个嵌套配置段 + `Load()` + `expandEnv()`
> - `validate.go` (261) — `Validate()` 多错合并校验 + `detectContextWindow()` 模型上下文自动识别 + MCP 命令白名单
>
> 测试：`validate_test.go` (113)
>
> 启动入口：`cmd/agent/main.go:initConfig` → `config.Load(configPath)` → `cfg.Validate()` → 失败即 `logger.Fatal`

---

## 1. 模块定位

**"agent 启动之前所有子系统的开关都在这一份 struct 里，错的一格都过不了 boot。"**

`internal/config` 的职责只有三条，但每一条都决定了系统在生产里的可运维性：

1. **统一配置来源**：YAML 文件 → 环境变量 → `SetDefault` → Go 零值，四层叠加由 Viper 完成；
2. **秘钥外置**：YAML 中以 `${VAR}` 占位符标记，`expandEnv()` 在加载完成后扫白名单字段调 `os.ExpandEnv`；
3. **快速失败**：`Validate()` 在 `main.go` 拿到 `*Config` 后立即跑一遍，把所有约束错误**一次合并报告**——没人想"改一个错跑一次"。

换句话说：**配置错误永远在 boot 阶段暴露，不会飘到运行时**。这条规则是整个 agent 启动顺序的根基（见 [00_overview](00_overview.md) §3 启动装配链）。

---

## 1.5 核心设计问题

### 为什么用 Viper 而不自己写 YAML+env 互转？

Viper 一次性搞定三件事：
- YAML/JSON/TOML 解析（我们只用 YAML）
- env 自动绑定（`AutomaticEnv` + `SetEnvKeyReplacer(".", "_")`）
- `mapstructure` 反序列化到 struct（我们的所有字段都带 `mapstructure:"..."` tag）

代价是 Viper 的字段查找走"内部 map"——`v.Get("llm.primary.model")` 是 map lookup，不是 struct 反射。这是它能做"env 实时覆盖"的代价；性能在 boot 时完全无所谓。

### 为什么四层覆盖而不是三层？

```
env var   > config.yaml > SetDefault > Go zero value
(最高优先)                                (最低)
```

四层都有不可替代的用途：

| 层 | 用例 |
|----|------|
| env var | Docker 部署，容器注入；同一镜像跨环境复用 |
| YAML | 本机开发；多字段一次编辑；带注释 |
| `SetDefault` | 合理默认（如 `server.http_addr=":8080"`），新加字段不强制每个环境都填 |
| Go 零值 | 兜底；保险丝；测试时 `&Config{}` 直接能用 |

去掉任何一层都会让另一层背负它本不该有的复杂度——比如去掉 `SetDefault` 就要求每个 YAML 都列全字段、每个 env 必须设置。

### 为什么每次启动做 Schema 校验？

Viper 只会"尽力反序列化"——字段缺失取零值，格式错误静默忽略。生产环境里 `sandbox.network_mode` 写错成 `"noneee"` 可能要等到第一次 sandbox 调用才报错——那时候客户已经看到错误了。

`Validate()` 在启动时 fail fast，让错误在 CI/CD 流水线就被拦。校验只做"配置自洽"，不做"下游依赖可达"——后者交给 `/readyz` 健康检查（[19_observability](19_observability.md)）。

### 为什么 `${VAR}` 展开必须**显式列字段**？

`os.ExpandEnv` 只对调用它的字段有效，不会自动遍历 struct。`config.go:354-379` 这段手动展开列表是出了名的"加新字段易漏"——P1 #20 修复前 `RAG.EmbeddingBaseURL` 没进白名单，导致字面量 `${OPENAI_BASE_URL}` 被传到下游当 URL，索引 POST 直接 400。

**当前覆盖 17 个标量字段 + MCP servers 的嵌套 URL/Command/Args/Env**：

```go
cfg.LLM.Primary.{APIKey, BaseURL}
cfg.LLM.Fallback.{APIKey, BaseURL}
cfg.RAG.{EmbeddingBaseURL, EmbeddingAPIKey, RerankBaseURL, RerankAPIKey}
cfg.Redis.{Addr, Password}
cfg.Postgres.DSN
cfg.Qdrant.Addr
cfg.Temporal.Host
cfg.Auth.JWTSecret
cfg.Tracing.Endpoint
for each MCP server: URL, Command, Args[*], Env[*]
```

**新加任何含 `${...}` 的字段都要同步更新这个清单。** §7 演进列表里有"用 reflect 自动遍历"的计划，但目前显式清单胜在**审计性**——code review 时一眼能看出哪些字段会被展开。

### 为什么 `AuthConfig.TokenExpiry/RefreshExpiry` 是 `string` 而不是 `time.Duration`？

历史遗留。Viper 反序列化 `time.Duration` 需要在 `mapstructure.Decode` 配 hook，旧版漏掉了；现在改成 Duration 是非破坏性改动（YAML 兼容，env 兼容），属于 §7 P0 待修项。当前值在 `main.go` 里被 `time.ParseDuration` 临时解析。

---

## 2. 公开类型总览

`Config` 是唯一根类型，18 个嵌套子结构：

```go
type Config struct {
    Server     ServerConfig     // HTTP/WS 监听、超时
    LLM        LLMConfig        // Primary + Fallback + CircuitBreaker
    Redis      RedisConfig      // 连接池
    Postgres   PostgresConfig   // DSN、连接池
    Qdrant     QdrantConfig     // 向量库地址、collection
    Temporal   TemporalConfig   // workflow server（当前为占位，见 [11_temporal]）
    Sandbox    SandboxConfig    // Docker host + cgroups + 镜像表
    MCP        MCPConfig        // MCP server 列表（含 PoolSize / Streaming 新字段）
    RAG        RAGConfig        // embedding/rerank API + cache + query rewrite
    Session    SessionConfig    // TTL、滑动窗口阈值、压缩模式
    Security   SecurityConfig   // 正则黑名单 + 出口 ACL（CIDR 在 internal/security）
    Logging    LoggingConfig    // zap level/format/output
    Auth       AuthConfig       // JWT secret、token TTL
    Tracing    TracingConfig    // OTel endpoint + 采样率
    PTY        PTYConfig        // P1 新增：持久 PTY 会话
    TreeSitter TreeSitterConfig // P1 新增：AST 解析开关
    LSP        LSPConfig        // P1 新增：LSP 客户端（占位实现）
    Workspace  WorkspaceConfig  // host workspace run_workspace_cmd 单次 exec 上限
}
```

每个字段都带 `mapstructure` tag 供 Viper 反序列化。下面只挑**有非显然语义**的几段展开。

### 2.1 `LLMProviderConfig` — 主备同构 + 上下文窗口自识别

```go
type LLMProviderConfig struct {
    Provider            string        // openai | anthropic | azure
    APIKey              string        // 支持 ${OPENAI_KEY}
    Model               string        // 例：gpt-4o-mini / claude-sonnet-4-6
    BaseURL             string        // 自建代理 / Azure endpoint / Ollama
    MaxTokens           int           // 单次响应最大 token
    Temperature         float32       // 0.0~2.0
    Timeout             time.Duration // 单次调用超时
    ContextWindow       int           // 0 → 由 detectContextWindow(Model) 自动识别
    EnablePromptCaching bool          // 向 Anthropic 兼容端发 cache_control 标记
}
```

**为什么 Primary 和 Fallback 同构？** 简化下游 `llm.router`——永远有两个 provider，不用判空。空 `Fallback.Model` 表示禁用 fallback（路由器内部判定）。

**`ContextWindow` 的自动识别**：`validate.go:142-179` 的 `detectContextWindow(model)` 维护了一张已知模型表（Claude 4.x/3.x = 200K，GPT-4o = 128K，Gemini 1.5 = 1M 等），用**前缀匹配**支持版本号变体（如 `claude-opus-4-20250514` → 200K）。识别失败默认 128K——足够大多数场景，但**会让"小窗口模型用得超"成为静默 bug**，所以 §7 列了"识别失败时 logger.Warn"作为改进。

**`EnablePromptCaching`**：Anthropic Messages API 的 `cache_control` 字段，命中后系统 prompt token 计费降到 10%——`llm.Client` 在序列化时根据这个开关决定是否注入 marker（[03_llm](03_llm.md) §prompt cache）。

### 2.2 `CircuitBreakerConfig` — 三个旋钮喂给 gobreaker

```go
type CircuitBreakerConfig struct {
    MaxFailures     int           // 默认 5：连续失败 N 次跳闸
    Timeout         time.Duration // 默认 30s：开路状态持续时间
    HalfOpenMaxReqs int           // 默认 2：半开探测并发数
}
```

直接喂给 `sony/gobreaker`，含义见 [03_llm](03_llm.md) §circuit breaker。

### 2.3 `MCPServerConfig` — stdio / sse 双形态 + 连接池

```go
type MCPServerConfig struct {
    Name      string
    Transport string            // "stdio" 或 "sse"
    Command   string            // stdio: 可执行文件
    Args      []string          // stdio: 参数
    URL       string            // sse: 端点
    Env       map[string]string // 会被 expandEnv 注入
    PoolSize  int               // ≥2 启用多子进程池（默认 0 = legacy 单连接）
    Streaming bool               // 启用 progress chunk 流到 SSE
}
```

**`PoolSize` 的含义**：单逻辑 MCP server 背后并发启动 N 个子进程，Gateway 做 "least-pending" 负载均衡——消除 stdin 序列化带来的吞吐瓶颈。典型值 3~4（受 MCP server 自身资源占用限制）。

**`Streaming`**：启用后 orchestrator 可调 `Gateway.CallToolStream` 拿 `<-chan ToolChunk`，把 MCP 的 `notifications/progress` 直接推到 SSE 前端；关闭时退化为 `CallTool` 单次阻塞返回。详见 [06_mcp](06_mcp.md) §streaming。

### 2.4 `RAGConfig` — embedding/rerank/cache/rewrite 一锅炖

```go
type RAGConfig struct {
    ChunkMaxTokens int
    OverlapTokens  int
    EmbeddingModel string

    EmbeddingProvider string  // "openai" | "local" | "" (auto)
    EmbeddingBaseURL  string
    EmbeddingAPIKey   string

    RerankEnabled bool
    RerankModel   string
    RerankBaseURL string
    RerankAPIKey  string
    TopK          int
    RerankTopN    int

    WatchEnabled bool   // 文件监视器 → 增量索引
    WatchPath    string

    EmbeddingCacheMode string // "memory" | "redis" | "tiered" | ""
    QueryRewriteMode   string // "none" | "hyde" | "expand"
}
```

四组开关解释：

| 字段 | 选项 | 用途 |
|------|------|------|
| `EmbeddingProvider` | openai / local / 空 | 空 = auto（缺 key 退本地 hash 模式） |
| `EmbeddingCacheMode` | memory / redis / tiered / 空 | tiered = L1 内存 + L2 Redis |
| `QueryRewriteMode` | none / hyde / expand | HyDE 走 LLM 生成假设文档；expand 做关键词扩展 |
| `WatchEnabled` | bool | 启用 fsnotify 监视 `WatchPath` 自动重建索引 |

每一组都对应 `rag` 包里的不同子模块——详见 [04_rag](04_rag.md)。

### 2.5 `SandboxConfig` — 安全三件套

```go
type SandboxConfig struct {
    DockerHost   string            // unix:///var/run/docker.sock
    DefaultImage string            // 默认镜像
    Images       map[string]string // language → image
    MemoryLimit  string            // "512m"
    CPULimit     string            // "1.0"
    Timeout      time.Duration     // 容器墙上时间
    NetworkMode  string            // "none" / "bridge" / 自建网络名
    WorkspaceDir string            // 挂载到容器内的项目目录
}
```

**默认 `NetworkMode="none"`**——沙箱内执行的代码默认零网络出口，避免数据外泄；要联网得显式改成 bridge 或挂自建网络（再配合 `SecurityConfig.EgressAllowedHosts`，见 §2.7）。

### 2.5.1 `WorkspaceConfig` — host 工作区 `run_workspace_cmd` 上限

```go
type WorkspaceConfig struct {
    CmdTimeout time.Duration // run_workspace_cmd 单次 exec 硬上限,0 = 5min 默认
}
```

**与 `SandboxConfig` 的区别**：`SandboxConfig.Timeout` 控的是 Docker 容器内 `run_tests` 的墙上时间；这里管的是 **host 进程直接 exec** 在 `/tmp/agent-workspaces/<id>` 下的 `run_workspace_cmd`（LLM 跑 `go test` / `pytest` / `npm test` / `bash test_*.sh` 的常用通道），无网络隔离仅有命令白名单（`validateWorkspaceCommand`）。

**两层钳制**：

1. 服务端兜底：`CmdTimeout`（默认 5min，通过 `orch.SetWorkspaceCmdTimeout` 由 `main.go` 注入）；
2. 调用方自选：LLM 可在 tool args 里传 `timeout_seconds`，effective = `min(timeout_seconds, CmdTimeout)`；传 0 或不传按 `CmdTimeout` 走。

超时后 `cmd.Cancel` 走 `syscall.Kill(-pgid, SIGKILL)` 杀掉整个进程组（包括 LLM 后台启的 server / sleep / curl），杜绝半截子进程驻留。从 2min 提到 5min 是为了覆盖典型的多阶段集成测试脚本（启动 server + 多组 curl 探针 + cleanup），既给合法长任务空间又不让一条命令把 ReAct loop 锁死半小时以上。

### 2.6 `SessionConfig` — 滑动窗口 + 压缩模式

```go
type SessionConfig struct {
    MaxHistoryTokens       int
    SummaryThresholdTokens int
    TTL                    time.Duration
    CompactionMode         string  // "truncate" (默认) | "summarize"
}
```

**`CompactionMode="summarize"`** 启用后超阈值不是直接截断，而是调 LLM 生成历史摘要保留——详见 [12_session](12_session.md) §压缩策略。代价是每次压缩多一次 LLM 调用，所以默认仍是 `truncate`。

### 2.7 `SecurityConfig` — 黑名单 + 出口 ACL

```go
type SecurityConfig struct {
    SensitivePatterns       []string  // 正则黑名单：命中触发 HITL approval
    RequireApprovalCommands []string  // 文本黑名单
    EgressEnabled           bool      // 是否启用 CIDR 出口过滤
    EgressAllowedHosts      []string  // 允许的目标 host:port
}
```

CIDR 解析在 `internal/security`（[18_auth_security](18_auth_security.md) §egress）。**注意 `Validate()` 只检查 SensitivePatterns 非空，不校验正则语法**——首次 `regexp.Compile` 时由 `internal/security` 报错。

### 2.8 `PTYConfig` / `TreeSitterConfig` / `LSPConfig` — P1 新增

```go
type PTYConfig struct {
    Enabled                 bool
    Backend                 string  // "docker" | "local"
    Image                   string  // backend=docker 时的镜像
    MaxSessionsPerWorkspace int     // 默认 3
    IdleTimeout             string  // 例 "5m"
    CommandTimeout          string  // 例 "120s"
    MemoryLimit             int64   // bytes
    CPUQuota                int64
    OutputLimit             int     // bytes，默认 1M
    Shell                   string  // 例 "/bin/bash"
}

type TreeSitterConfig struct {
    Enabled     bool
    Languages   []string  // ["go","python","typescript",...]
    MaxFileSize int       // bytes
}

type LSPConfig struct {
    Enabled               bool
    Servers               map[string]LSPServerConfig  // {"gopls": {...}, "pyright": {...}}
    InitializationTimeout string                       // "30s"
    RequestTimeout        string                       // "5s"
    MaxConcurrentRequests int
}
```

三个新模块的详细设计见 [26_pty](26_pty.md) / [24_treesitter](24_treesitter.md) / [27_lsp](27_lsp.md)。**LSP 当前仍是占位实现**（接口完备但方法返回桩值），文档保留以便后续接通。

---

## 2.5 数据流总览

```text
═══════════════════════ 启动时配置装载 ═══════════════════════

main.go:initConfig()
   │
   ▼
┌──────────────────────────────────────────────┐
│  config.Load(configPath)                     │
│                                              │
│  1. viper.New()                              │
│  2. SetDefault("server.http_addr", ":8080")  │  ← 第 4 层
│  3. configPath != "" ?                       │
│       yes → SetConfigFile(configPath)        │
│       no  → AddConfigPath("./configs",       │
│                 "/etc/code-agent/configs")   │
│  4. SetEnvPrefix("CODE_AGENT")               │
│     SetEnvKeyReplacer(".","_")               │
│     AutomaticEnv()                           │  ← 第 1 层（最高）
│  5. ReadInConfig()                           │  ← 第 2 层（YAML）
│  6. Unmarshal(&cfg)                          │
│  7. expandEnv() × 17 字段 + MCP 嵌套         │  ← ${VAR} 展开
│                                              │
│  return &cfg                                 │
└──────────────────┬───────────────────────────┘
                   │ *config.Config
                   ▼
┌──────────────────────────────────────────────┐
│  cfg.Validate()                              │
│   ├─ server / llm / redis / session / rag    │
│   ├─ sandbox / security                      │
│   ├─ MCP commands 白名单 + args 黑名单       │
│   └─ detectContextWindow(Model)              │
│                                              │
│  errs []string ← 全部收集                    │
│  len(errs)>0 → return 合并 error             │
└──────────────────┬───────────────────────────┘
                   │
        ┌──────────┴──────────┐
        │ ok                  │ err
        ▼                     ▼
   下游消费者              logger.Fatal
   各取所需字段
        │
        ▼
   cfg.LLM    → llm.NewClient(cfg.LLM)
   cfg.RAG    → rag.NewEngine(cfg.RAG, embedClient, qdrantClient)
   cfg.Redis  → session.NewRedisManager(cfg.Redis, ...)
   cfg.Sandbox → sandbox.NewManager(cfg.Sandbox)
   cfg.PTY    → pty.NewManager(cfg.PTY)
   ...
```

---

## 3. `Load()` 加载流程

```go
func Load(configPath string) (*Config, error) {
    v := viper.New()

    // (1) 默认值——只列两条示例，实际更多
    v.SetDefault("server.http_addr", ":8080")
    v.SetDefault("server.ws_addr", ":8081")
    v.SetDefault("server.read_timeout", "30s")
    v.SetDefault("server.write_timeout", "60s")
    v.SetDefault("server.shutdown_timeout", "15s")
    v.SetDefault("logging.level", "info")
    v.SetDefault("logging.format", "json")

    // (2) 配置文件位置
    if configPath != "" {
        v.SetConfigFile(configPath)
    } else {
        v.SetConfigName("config")
        v.SetConfigType("yaml")
        v.AddConfigPath("./configs")
        v.AddConfigPath("/etc/code-agent/configs")
    }

    // (3) env 覆盖
    v.SetEnvPrefix("CODE_AGENT")
    v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
    v.AutomaticEnv()

    // (4) 读 + 反序列化
    if err := v.ReadInConfig(); err != nil { ... }
    var cfg Config
    if err := v.Unmarshal(&cfg); err != nil { ... }

    // (5) ${VAR} 展开（白名单字段）
    cfg.LLM.Primary.APIKey  = expandEnv(cfg.LLM.Primary.APIKey)
    cfg.LLM.Primary.BaseURL = expandEnv(cfg.LLM.Primary.BaseURL)
    // ... 17 个标量字段
    for i := range cfg.MCP.Servers {
        cfg.MCP.Servers[i].URL     = expandEnv(...)
        cfg.MCP.Servers[i].Command = expandEnv(...)
        for j, arg := range cfg.MCP.Servers[i].Args  { expandEnv(arg) }
        for k, val := range cfg.MCP.Servers[i].Env   { expandEnv(val) }
    }

    return &cfg, nil
}
```

### 3.1 环境变量映射规则

| YAML key                      | 对应 ENV 变量                      |
|-------------------------------|------------------------------------|
| `server.http_addr`            | `CODE_AGENT_SERVER_HTTP_ADDR`      |
| `llm.primary.model`           | `CODE_AGENT_LLM_PRIMARY_MODEL`     |
| `llm.primary.api_key`         | `CODE_AGENT_LLM_PRIMARY_API_KEY`   |
| `redis.addr`                  | `CODE_AGENT_REDIS_ADDR`            |
| `rag.embedding_cache_mode`    | `CODE_AGENT_RAG_EMBEDDING_CACHE_MODE` |

规则：**全部大写 + `.` 换 `_`**，由 `SetEnvKeyReplacer` 自动转换。

**Viper 嵌套 map 的 env 覆盖支持差**——`MCP.Servers[*].Env` 这类嵌套 map 用 env 改是不可行的（Viper 不解析 `[*]` 语法）。要改这些只能改 YAML 或加自定义 unmarshal hook。

### 3.2 `${VAR}` 展开的语义陷阱

```yaml
llm:
  primary:
    api_key: "${OPENAI_API_KEY}"   # 不落盘明文
auth:
  jwt_secret: "${JWT_SECRET}"
```

`expandEnv()` 实现极简：

```go
func expandEnv(s string) string {
    if strings.Contains(s, "${") {
        return os.ExpandEnv(s)
    }
    return s
}
```

**未设置的环境变量被替换成空串**——这是设计的而非缺陷：让下游的"为空则降级"逻辑（如 RAG embedder 缺 key 时退本地 hash 模式）能正常触发。如果想强制要求设置，由 `Validate()` 检查"空串触发错误"——只有非空才能通过启动。

---

## 4. `Validate()` 校验

### 4.1 校验哲学：一次报全

```go
func (c *Config) Validate() error {
    var errs []string
    // 对每条规则：if 失败就 append(errs, "xxx is required")
    if len(errs) > 0 {
        return fmt.Errorf("configuration validation failed:\n  - %s",
                          strings.Join(errs, "\n  - "))
    }
    return nil
}
```

**为什么不短路**：开发者改完 `llm.primary.api_key` 想再启动，结果立刻看到 `redis.addr` 又缺——只能"改一个跑一次"。多错合并报告后开发者一次性能看到所有缺失项。

### 4.2 当前校验清单

| 检查点 | 规则 |
|--------|------|
| `server.http_addr` | 非空 |
| `server.read/write_timeout` | > 0 |
| `llm.primary.model` | 非空 |
| `llm.primary.{api_key,base_url}` | 两者至少一个非空 |
| `llm.primary.max_tokens` | > 0 |
| `llm.primary.timeout` | > 0 |
| `llm.primary.context_window` | 0 → 调 `detectContextWindow(model)` 自动填 |
| `llm.fallback.model != ""` 时 fallback.context_window | 同上自动填 |
| `llm.circuit_breaker.max_failures` | > 0 |
| `redis.addr` + `pool_size` | 非空 / > 0 |
| `session.max_history_tokens` | > 0 |
| `session.summary_threshold_tokens` | > 0 且 ≤ max_history_tokens |
| `session.ttl` | > 0 |
| `rag.chunk_max_tokens` + `top_k` | > 0 |
| `sandbox.timeout` | > 0 |
| `security.sensitive_patterns[i]` | 非空字符串（**未校验正则语法**） |
| `mcp.servers[i].command` | 非空 + 在 `allowedMCPCommands` 白名单 |
| `mcp.servers[i].args` | 不含 `--eval/-e/-c/eval/exec` 等危险参数 |

**白名单 MCP 命令**（`validate.go:182-192`）：

```go
allowedMCPCommands = {
    "npx", "node", "python", "python3",
    "uvx", "uv", "deno", "bun", "docker"
}
```

**为什么白名单**：MCP server 的 `command` 字段直接 fork 子进程——如果允许任意命令，YAML 注入 = RCE。白名单 + 危险参数黑名单两道防线把"通过配置文件远程执行"的攻击面收到几乎为零。

**绝对路径的额外约束**：如果 `command` 是绝对路径（如 `/usr/local/bin/node`），还要在 `{/usr/bin/, /usr/local/bin/, /opt/homebrew/bin/}` 之一下——不能指向 `/tmp/evil`。

### 4.3 `detectContextWindow()` 模型识别

```go
modelWindows = {
    "claude-opus-4-7":     200000,
    "claude-sonnet-4-6":   200000,
    "claude-3-5-sonnet":   200000,
    "gpt-4o":              128000,
    "gpt-4":               8192,
    "gemini-1.5-pro":      1000000,
    // ...
}
```

精确匹配优先，失败走前缀匹配（支持 `claude-opus-4-20250514` → 200K）。识别失败默认 128K——足够大多数场景，但**会让"小窗口模型用得超"成为静默 bug**（识别失败 ≠ Validate 失败）。§7 列了"识别失败 logger.Warn"作为改进。

### 4.4 已知未校验项

- `Temporal.Host` 格式（`host:port`）
- `Tracing.Endpoint` 格式
- `Security.SensitivePatterns` 正则语法（首次 Compile 时报错）
- `Postgres.DSN` 非空（store 模块允许空 = 禁用 PG）
- `Qdrant.Addr` 格式

这些属于"能跑但行为诡异"类，应陆续加上——见 §7 P0 列表。

---

## 5. 典型 YAML 示例

`configs/config.yaml`（涵盖 P1 新增字段的精简版）：

```yaml
server:
  http_addr: ":8080"
  ws_addr: ":8081"
  read_timeout: 30s
  write_timeout: 600s            # ReAct 多步循环可能跑几分钟
  shutdown_timeout: 15s

llm:
  primary:
    provider: anthropic
    api_key: "${ANTHROPIC_API_KEY}"
    model: claude-sonnet-4-6
    base_url: "https://api.anthropic.com"
    max_tokens: 8192
    temperature: 0.2
    timeout: 60s
    context_window: 0            # auto-detect → 200000
    enable_prompt_caching: true  # Anthropic cache_control
  fallback:
    provider: openai
    model: gpt-4o-mini
    base_url: "http://localhost:11434/v1"   # 本地 ollama
    api_key: "ollama"
    timeout: 120s
  circuit_breaker:
    max_failures: 5
    timeout: 30s
    half_open_max_requests: 2

redis:
  addr: "localhost:6379"
  pool_size: 50
  min_idle_conns: 5

postgres:
  dsn: "${POSTGRES_DSN}"
  max_open_conns: 25
  max_idle_conns: 5
  conn_max_lifetime: 5m

qdrant:
  addr: "localhost:6334"
  collection: "code_chunks"
  vector_size: 1536
  timeout: 10s

sandbox:
  docker_host: "unix:///var/run/docker.sock"
  default_image: "code-agent/sandbox-base:latest"
  memory_limit: "512m"
  cpu_limit: "1.0"
  timeout: 60s
  network_mode: "none"           # 默认零网络

workspace:
  cmd_timeout: 5m                # host run_workspace_cmd 单次 exec 上限;LLM tool 可在 [0, 此值] 内传 timeout_seconds 自定义更短

mcp:
  servers:
    - name: "filesystem"
      transport: "stdio"
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
      env:
        DEBUG: "false"
      pool_size: 3                # 启用 3 子进程池
      streaming: false

rag:
  chunk_max_tokens: 512
  overlap_tokens: 64
  top_k: 20
  rerank_enabled: true
  rerank_top_n: 5
  embedding_provider: "openai"
  embedding_base_url: "https://api.openai.com/v1"
  embedding_api_key: "${OPENAI_API_KEY}"
  embedding_model: "text-embedding-3-small"
  embedding_cache_mode: "tiered" # L1 mem + L2 redis
  query_rewrite_mode: "hyde"     # 启用 HyDE 改写
  watch_enabled: true
  watch_path: "/workspace"

session:
  max_history_tokens: 8000
  summary_threshold_tokens: 4000
  ttl: 24h
  compaction_mode: "summarize"   # 超阈值调 LLM 摘要而非截断

security:
  sensitive_patterns:
    - "DROP\\s+DATABASE"
    - "rm\\s+-rf\\s+/"
    - "kubectl\\s+delete"
  require_approval_commands:
    - "git push --force"
  egress_enabled: true
  egress_allowed_hosts:
    - "api.openai.com:443"
    - "api.anthropic.com:443"

auth:
  enabled: true
  jwt_secret: "${JWT_SECRET}"
  jwt_issuer: "code-agent"
  token_expiry: "24h"
  refresh_expiry: "168h"

tracing:
  enabled: true
  endpoint: "localhost:4317"
  service_name: "code-agent"
  sample_rate: 0.1
  insecure: true

pty:                              # P1 新增
  enabled: true
  backend: "local"
  max_sessions_per_workspace: 3
  idle_timeout: "5m"
  command_timeout: "120s"
  output_limit: 1048576
  shell: "/bin/bash"

tree_sitter:                      # P1 新增
  enabled: true
  languages: ["go","python","typescript","javascript","rust","java","c","cpp"]
  max_file_size: 1048576

lsp:                              # P1 新增（占位实现）
  enabled: false
  servers:
    gopls:
      command: "gopls"
      args: []
      languages: ["go"]
  initialization_timeout: "30s"
  request_timeout: "5s"
  max_concurrent_requests: 8
```

---

## 6. 设计权衡

| 抉择 | 理由 |
|------|------|
| Viper 而非手写 YAML+env | 避免维护 mapstructure + env 互转；Go 生态事实标准 |
| 秘钥**不**集成 Vault/KMS | 放低耦合；部署侧用 `docker secrets` / `envFrom` 注入 env 即可 |
| `Validate()` 一次返回全部错误 | 改 YAML 时能一次看全，不用反复重启 |
| Fallback 与 Primary **同构** | 简化 `llm.router`：永远有两个 provider，不用判空 |
| 只在敏感字段调用 `expandEnv` | 安全 + 明确边界：避免某个字段写了 `$PATH` 被误展开 |
| MCP `command` 白名单 + args 黑名单 | 配置文件即 RCE 风险面；双层防御 |
| `EmbeddingCacheMode` 多档而非布尔 | memory/redis/tiered 各有适用场景，二值开关不够 |
| `CompactionMode` 默认 `truncate` | summarize 多一次 LLM 调用；默认偏 cheap |
| Sandbox `NetworkMode` 默认 `none` | 安全默认值；要联网必须显式改 |
| `ContextWindow=0` 触发自动识别 | YAML 不用写死；新模型加进 detect 表即可 |

---

## 7. 后续演进

P0（影响生产可运维性）：

- [ ] `Auth.TokenExpiry/RefreshExpiry` 改 `time.Duration`，让 Viper 自动解析（避免 main.go 临时 ParseDuration）
- [ ] `Validate()` 覆盖更多字段：`Postgres.DSN` 非空时格式合法、`Qdrant.Addr` 格式、`Temporal.Host` 格式
- [ ] `detectContextWindow()` 识别失败时 `logger.Warn`——避免"未知模型被默认 128K 截断"成为静默 bug

P1（开发体验）：

- [ ] `expandEnv()` 改 reflect 自动遍历所有 string 字段——消除"新字段易漏白名单"的固有风险
- [ ] 配置 redact 方法（`String() string` 实现）让 `zap.Any("config", cfg)` 自动屏蔽密钥
- [ ] 导出 JSON Schema 供前端"设置页"实时校验提示

P2（可选）：

- [ ] **热重载**：`viper.WatchConfig` + 模块订阅机制，允许 `llm.temperature` / `security.sensitive_patterns` 等运行时参数 in-place 更新（注意不是所有字段都能热改——Redis.Addr 改了要重连，复杂度上升）
- [ ] **本地覆盖**：约定 `configs/local.yaml` 在 git 忽略，`Load` 时优先合并覆盖 `config.yaml`——开发者本地小改不污染主配置

---

## 8. 与其他模块的边界

### 8.1 上游：`main.go`

```go
// cmd/agent/main.go:initConfig
cfg, err := config.Load(configPath)
if err != nil { logger.Fatal("config load", err) }
if err := cfg.Validate(); err != nil { logger.Fatal("config invalid", err) }
return cfg
```

`main.go` 拿到 `*Config` 后**按 section 分发给各子系统构造函数**——没有任何模块直接读 YAML，全部走 `*Config` 字段。这条边界让"换配置后端"（如 etcd / Consul）只需要改 `Load()` 一处。

### 8.2 下游：所有子系统

- `cfg.LLM` → `llm.NewClient`（[03_llm](03_llm.md)）
- `cfg.RAG` → `rag.NewEngine`（[04_rag](04_rag.md)）
- `cfg.Sandbox` → `sandbox.NewManager`（[05_sandbox](05_sandbox.md)）
- `cfg.MCP` → `mcp.NewGateway`（[06_mcp](06_mcp.md)）
- `cfg.Redis` + `cfg.Session` → `session.NewRedisManager`（[12_session](12_session.md)）
- `cfg.Sandbox.WorkspaceDir` / `cfg.PTY` → `workspace.NewManager` + `pty.NewManager`（[14_workspace](14_workspace.md) / [26_pty](26_pty.md)）
- `cfg.Security` → `security.NewSensitiveDetector` + egress filter（[18_auth_security](18_auth_security.md)）
- `cfg.Tracing` → `tracing.InitTracer`（[19_observability](19_observability.md)）

---

## 9. 设计教训

这个模块经过三次重构才稳定到当前形态：

1. **第一次**：手写 YAML + env 解析，每加一个字段要改三处。Viper 引入是常识。
2. **第二次**：`expandEnv` 用 reflect 自动遍历——结果某个字段名拼写错 `${LLm_KEY}` 仍被替换成空串（reflect 找不到环境变量但不报错），调试两小时。改回白名单显式列举。
3. **第三次**：`Validate()` 一开始是短路返回——第一个错就 return。开发者反馈"我改了 5 个字段，启动了 5 次"。改成多错合并是这个反馈的直接产物。

教训：**配置代码的"轻量优雅"诱惑很大，但每一次"省一行代码"的代价都是运维体验的退步**。白名单显式列字段、`Validate` 跑全量、`Load` 失败立即 Fatal——这些"不优雅"的写法换来的是 boot 阶段就把所有问题暴露。

---

下一篇：[`02_models.md`](02_models.md) —— 跨模块共享的领域类型。
