# 01 · 配置模块 `internal/config`

> 代码：`internal/config/config.go` (254 行) + `internal/config/validate.go` (87 行)
> 测试：`internal/config/validate_test.go` (113 行)

---

## 1. 模块定位

所有子系统启动前必须拿到一个 **已校验的 `*config.Config`**。该模块的职责：

1. **统一配置来源**：YAML 文件 → 环境变量 → 默认值，三层叠加；
2. **秘钥外置**：所有敏感字段支持 `${ENV_VAR}` 占位符展开；
3. **快速失败**：启动时一次性校验所有必填/范围约束，输出多错误合并报告。

换句话说：**配置错误永远在 boot 阶段暴露，不会飘到运行时。**

---

## 1.5 核心设计问题

### 为什么不用 JSON / TOML 而用 YAML？

YAML 对手写最友好——支持注释、缩进表达层级、字符串不强制引号。我们的
配置经常有 `# Override via CODE_AGENT_XXX`、`# See docs/04_rag.md` 这类
注释，YAML 原生支持。

**代价**：YAML 缩进歧义（tab vs space）、不支持注释-保留的 round-trip
编辑。运维脚本改配置时用 `yq` 而非直接 sed。

### 为什么四层覆盖？

```
env var   > config.yaml > SetDefault > Go zero value
(最高优先)                                (最低)
```

**必要性**：
- Docker 部署用 env（容器注入）
- 本机开发用 YAML
- 新字段有合理默认就不用每个环境都配
- 零值是兜底（字段类型的保险丝）

### 为什么每次启动做 Schema 校验？

Viper 只会尽力反序列化，字段缺失取零值，格式错误静默忽略。生产里
`sandbox.network_mode` 写错成 `noneee` 可能要等第一次 sandbox 调用
才报错——那时候客户已经看到错误了。`Validate()` 在启动时 fail fast，
让错误在部署流水线就被拦。

### 为什么 ${VAR} 展开必须显式列字段

`os.ExpandEnv` 只对调用它的字段有效。P1 #20 修复前 `RAG.EmbeddingBaseURL`
没进白名单 → 字面量 `${VAR}` 被传下游用作 URL → 请求失败。修复后覆盖
13+ 字段，**新加任何含 `${...}` 的字段都要同步更新 expandEnv 清单**。

---

## 2. 公开类型总览

`Config` 是唯一根类型，其他都是它的嵌套子结构：

```go
type Config struct {
    Server   ServerConfig   // HTTP/WS 监听、超时
    LLM      LLMConfig      // Primary + Fallback + CircuitBreaker
    Redis    RedisConfig    // 连接池、哨兵
    Postgres PostgresConfig // DSN、连接池
    Qdrant   QdrantConfig   // 向量库地址、collection
    Temporal TemporalConfig // workflow server
    Sandbox  SandboxConfig  // Docker host + cgroups + 镜像表
    MCP      MCPConfig      // MCP server 列表 (stdio | sse)
    RAG      RAGConfig      // embedding/rerank API、chunk/topK
    Session  SessionConfig  // TTL、滑动窗口阈值
    Security SecurityConfig // 正则黑名单、出口白名单
    Logging  LoggingConfig  // zap level/format
    Auth     AuthConfig     // JWT secret、token TTL
    Tracing  TracingConfig  // OTel endpoint
}
```

> 每个字段都带 `mapstructure` tag，供 Viper 反序列化使用。

### 2.1 LLMProviderConfig — 主备一致

```go
type LLMProviderConfig struct {
    Provider    string        // openai | azure | anthropic
    APIKey      string        // 支持 ${OPENAI_KEY}
    Model       string        // 例：gpt-4o
    BaseURL     string        // 自建代理/Azure endpoint
    MaxTokens   int
    Temperature float32
    Timeout     time.Duration
}
```

`LLMConfig` 内含 `Primary + Fallback`，两个结构**同构**——正是 `llm/router.go` 能无痛切换的基础。

### 2.2 CircuitBreakerConfig

```go
type CircuitBreakerConfig struct {
    MaxFailures     int
    Timeout         time.Duration
    HalfOpenMaxReqs int
}
```

直接喂给 `sony/gobreaker`。详见 `03_llm.md`。

### 2.3 MCPServerConfig — stdio / sse 双形态

```go
type MCPServerConfig struct {
    Name      string
    Transport string            // "stdio" 或 "sse"
    Command   string            // stdio: 可执行文件
    Args      []string          // stdio: 参数
    URL       string            // sse: 端点
    Env       map[string]string // 会被 expandEnv 注入
}
```

### 2.4 SandboxConfig — 安全三件套

```go
type SandboxConfig struct {
    DockerHost   string            // unix:///var/run/docker.sock
    DefaultImage string            // 默认镜像
    Images       map[string]string // language → image
    MemoryLimit  string            // "512m"
    CPULimit     string            // "1.0"
    Timeout      time.Duration     // 容器墙上时间
    NetworkMode  string            // "none" 或白名单网络名
    WorkspaceDir string            // 挂载到容器内的项目目录
}
```

---

## 2.5 数据流总览

```text
┌─────────────────────────────────────────────────────────────────┐
│                      Load(configPath)                            │
└─────────────────────────────┬───────────────────────────────────┘
                              │
        ┌─────────────────────┼─────────────────────┐
        ▼                     ▼                     ▼
┌──────────────┐    ┌──────────────────┐   ┌────────────────────┐
│ config.yaml  │    │ 环境变量          │   │ SetDefault(硬编码) │
│  (YAML文件)  │    │ CODE_AGENT_*     │   │ pool_size=50 等    │
└──────┬───────┘    └────────┬─────────┘   └─────────┬──────────┘
       │                     │                       │
       └─────────────────────┼───────────────────────┘
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│              Viper 四层优先级合并                                 │
│  env var > config.yaml > SetDefault > Go zero value             │
│                                                                  │
│  SetConfigFile → SetEnvPrefix → AutomaticEnv → ReadInConfig     │
│  → Unmarshal(&cfg)                                              │
└─────────────────────────────┬───────────────────────────────────┘
                              │ (raw *Config)
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    expandEnv()                                    │
│  遍历 13+ 敏感字段，将 ${VAR} 替换为 os.Getenv 实际值            │
│  涉及：APIKey / DSN / RedisAddr / HMACSecret / ...              │
└─────────────────────────────┬───────────────────────────────────┘
                              │ (*Config, 已展开)
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Validate()                                     │
│  多错误收集模式(不短路)                                           │
│  检查：端口范围 / 必填字段 / 枚举值 / 阈值合法性                  │
└──────────────┬──────────────────────────────────┬───────────────┘
               │                                  │
               ▼                                  ▼
      ┌────────────────┐                 ┌────────────────────┐
      │  校验通过       │                 │  校验失败           │
      │  return &cfg   │                 │  return joined err │
      └────────┬───────┘                 └────────────────────┘
               │
               ▼ (*config.Config)
┌─────────────────────────────────────────────────────────────────┐
│  下游消费者（按 section 拆分读取）：                              │
│  cfg.LLM     → llm.Client                                      │
│  cfg.RAG     → rag.Engine                                       │
│  cfg.Sandbox → sandbox.Manager                                  │
│  cfg.Redis   → session.Manager                                  │
│  cfg.Server  → api.Server                                       │
└─────────────────────────────────────────────────────────────────┘
```

---

## 3. 加载流程 `Load()`

```
┌────────────────────────────────────────┐
│ Load(configPath string) *Config        │
└─┬──────────────────────────────────────┘
  │ 1. viper.New() 设置默认值
  │ 2. 选择配置文件
  │    - 显式 path → SetConfigFile
  │    - 否则搜索 ./configs, /etc/code-agent/configs
  │ 3. SetEnvPrefix("CODE_AGENT")
  │    + Replacer(. → _)
  │    + AutomaticEnv()
  │ 4. ReadInConfig()
  │ 5. v.Unmarshal(&cfg)  (mapstructure)
  │ 6. expandEnv() 展开敏感字段中的 ${VAR}
  │    - LLM.Primary.APIKey
  │    - RAG.EmbeddingAPIKey / RerankAPIKey
  │    - Redis.Password
  │    - Postgres.DSN
  │    - Auth.JWTSecret
  │    - MCP.Servers[*].Env 每个 value
  │ 7. return &cfg, nil
  ▼
```

### 3.1 环境变量映射规则

| YAML key                      | 对应 ENV 变量                      |
|-------------------------------|------------------------------------|
| `server.http_addr`            | `CODE_AGENT_SERVER_HTTP_ADDR`      |
| `llm.primary.model`           | `CODE_AGENT_LLM_PRIMARY_MODEL`     |
| `redis.addr`                  | `CODE_AGENT_REDIS_ADDR`            |

规则简单：**全部大写 + `.` 换 `_`**，由 `SetEnvKeyReplacer` 自动转换。

### 3.2 敏感字段的 `${VAR}` 展开

```yaml
# configs/config.yaml 片段
llm:
  primary:
    api_key: "${OPENAI_API_KEY}"   # 不落盘明文
auth:
  jwt_secret: "${JWT_SECRET}"
```

启动时 `expandEnv()` 用 `os.ExpandEnv` 替换。**设计约定**：

- 只对预设白名单字段展开（不是对所有字段），避免用户意外引入 `$` 被当成变量；
- 未设置的环境变量会被替换成空串，随后被 `Validate()` 拦截 → 启动失败。

---

## 4. 校验 `Validate()`

位置：`internal/config/validate.go`。
设计亮点 —— **一次收集所有错误**，不要 "改一个错等下一次启动才发现下一个"：

```go
func (c *Config) Validate() error {
    var errs []string
    // ... 每个字段 if 失败就 append
    if len(errs) > 0 {
        return fmt.Errorf(
            "configuration validation failed:\n  - %s",
            strings.Join(errs, "\n  - "))
    }
    return nil
}
```

### 4.1 校验清单（节选）

| 检查点 | 规则 |
|---|---|
| `server.http_addr` | 非空 |
| `server.read/write_timeout` | > 0 |
| `llm.primary.model` | 非空 |
| `llm.primary.{api_key,base_url}` | 两者至少一个非空 |
| `llm.primary.max_tokens` | > 0 |
| `llm.primary.timeout` | > 0 |
| `llm.circuit_breaker.max_failures` | > 0 |
| `redis.addr` + `pool_size` | 非空 / > 0 |
| `session.max_history_tokens` | > 0 |
| `session.summary_threshold_tokens` | > 0 且 ≤ max_history_tokens |
| `session.ttl` | > 0 |
| `rag.chunk_max_tokens` + `top_k` | > 0 |
| `sandbox.timeout` | > 0 |
| `security.sensitive_patterns[i]` | 非空字符串 |

> 未检：正则的**语法正确性** —— 这由 `internal/security` 中使用者首次 `regexp.Compile` 时报错。
> 一致性原则：校验层只做"配置合法"，不做"下游依赖可达"，后者交给健康检查。

### 4.2 测试

`validate_test.go` 为每条规则写了一例"成功 + 失败"，覆盖率高。核心用法：

```go
cfg := buildValidConfig()
cfg.Session.SummaryThresholdTokens = cfg.Session.MaxHistoryTokens + 1
err := cfg.Validate()
require.Error(t, err)
require.Contains(t, err.Error(), "must be <= max_history_tokens")
```

---

## 5. 典型 YAML 示例

`configs/config.yaml`（精简版）：

```yaml
server:
  http_addr: ":8080"
  read_timeout: 30s
  write_timeout: 60s

llm:
  primary:
    provider: openai
    api_key: "${OPENAI_API_KEY}"
    model: gpt-4o-mini
    base_url: "https://api.openai.com/v1"
    max_tokens: 4096
    temperature: 0.2
    timeout: 60s
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

qdrant:
  addr: "localhost:6334"
  collection: "code_chunks"
  vector_size: 1536

sandbox:
  docker_host: "unix:///var/run/docker.sock"
  default_image: "code-agent/sandbox-base:latest"
  memory_limit: "512m"
  cpu_limit: "1.0"
  timeout: 60s
  network_mode: "none"

rag:
  chunk_max_tokens: 512
  top_k: 20
  embedding_base_url: "https://api.openai.com/v1"
  embedding_api_key: "${OPENAI_API_KEY}"
  embedding_model: "text-embedding-3-small"

session:
  max_history_tokens: 8000
  summary_threshold_tokens: 4000
  ttl: 24h

security:
  sensitive_patterns:
    - "DROP\\s+DATABASE"
    - "kubectl\\s+delete"

auth:
  jwt_secret: "${JWT_SECRET}"
  token_expiry: "24h"
  refresh_expiry: "168h"

tracing:
  enabled: true
  endpoint: "localhost:4317"
  service_name: "code-agent"
  sample_rate: 0.1
```

---

## 6. 设计权衡

| 抉择 | 理由 |
|---|---|
| 选择 Viper 而非手写 YAML+env | 避免自己维护 mapstructure + env 互转；Viper 是 Go 生态事实标准 |
| 秘钥**不支持** Vault/KMS 集成（在本层） | 放低耦合；部署侧用 `docker secrets` / `envFrom` 注入环境变量即可 |
| 校验**一次返回全部错误** | 开发者改 YAML 时能一次看全问题，而不是反复重启 |
| Fallback 与 Primary **同构而不是可选** | 简化下游：`router` 永远有两个 provider，不用判空 |
| 只在敏感字段调用 `expandEnv` | 安全与明确边界：避免"某个字段因为写了 `$PATH` 被误展开" |

---

## 7. 后续演进

- [ ] 支持 **热重载**（配合 `viper.WatchConfig`），对 LLM temperature、sensitive_patterns 等运行时参数允许 in-place 更新；
- [ ] 引入 **JSON Schema** 导出，便于前端"设置页"对字段做实时提示；
- [ ] 对 secret 字段加 **redact** 方法，避免 `zap.Any("config", cfg)` 把密钥写进日志；
- [ ] 与 `config/local.yaml` 约定 `.gitignore`，并在 `Load` 时优先合并 `local.yaml` 覆盖 `config.yaml`。

---

## 10. 实现剖析与改进方向

### Viper 装载的 5 步
```text
1. SetConfigFile(*configPath)       — 定位 YAML 文件（--config 或默认 configs/config.yaml）
2. SetEnvKeyReplacer(".","_")       — 把 llm.primary.api_key 映射到 LLM_PRIMARY_API_KEY
3. SetEnvPrefix("CODE_AGENT")       — 只认 CODE_AGENT_* 前缀，避免污染
4. AutomaticEnv()                   — 每次 Get 都查 env
5. ReadInConfig() → Unmarshal       — YAML 解析到 struct
   之后 expandEnv(&cfg) 扫 13+ 字段把 ${VAR} 展开
```

### Pros
- ✅ 四层覆盖（env > yaml > default > zero）
- ✅ Validate() 启动 fail-fast
- ✅ expandEnv 覆盖 13+ 字段（P1 #20 修复后）

### Cons
- ⚠️ AuthConfig.TokenExpiry / RefreshExpiry 类型是 string 但未 parse
- ⚠️ expandEnv 白名单需要手动维护（加新字段容易漏）
- ⚠️ 嵌套 map（如 MCP env map[string]string）的 env 覆盖支持差

### 改进方向
- **P0** — `TokenExpiry` 改 `time.Duration` 并让 viper 自动解析
- **P0** — Validate 覆盖更多字段（Postgres.DSN 非空、Qdrant.Addr 格式）
- **P1** — expandEnv 改 reflect 自动遍历所有 string 字段
- **P2** — 热加载（SIGHUP 重新读取配置，更新运行时行为）

---

下一篇：`02_models.md` —— 跨模块共享的领域类型（`internal/models`）。
