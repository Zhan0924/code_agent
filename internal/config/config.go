// Package config provides configuration loading and validation for the Code Agent.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【四层配置优先级（高到低）】
//
//	1. 环境变量  CODE_AGENT_<SECTION>_<KEY>（如 CODE_AGENT_REDIS_ADDR）
//	2. YAML 文件 configs/config.yaml（或 --config 参数指定）
//	3. 默认值    硬编码在 Load() 里的 Viper SetDefault
//	4. 零值      Go struct 零值（仅当上面都没命中）
//
// 【为什么用 Viper】
//
//	Viper 一次性搞定三件事：YAML 解析、env 自动绑定（AutomaticEnv + EnvKeyReplacer）、
//	mapstructure 反序列化。我们只需要写 struct tag `mapstructure:"..."`。
//
// 【${VAR} 展开的陷阱】
//
//	YAML 里写 `api_key: ${CODE_AGENT_LLM_API_KEY}` 时，Viper 不会自动展开。
//	必须调用 expandEnv() 把字面量 `${VAR}` 替换成 os.Getenv 的值。**必须
//	覆盖所有可能出现 ${} 的字段**——遗漏一个就会把字面量当真实 URL/Key 用。
//	P1 #20 修复前 RAG.EmbeddingBaseURL 被遗漏，导致索引时 POST 到字面量
//	URL 直接 400。现在 expandEnv 覆盖了 13+ 字段，见 Load() 函数尾部。
//
// 【零期望的默认值】
//
//	几乎每一项都有合理默认：
//	  · Server.HTTPAddr = ":8080"
//	  · LLM.CircuitBreaker = {MaxFailures:5, Timeout:30s, HalfOpenMaxReqs:2}
//	  · Session.TTL = 24h
//	  · Sandbox.NetworkMode = "none"（最严格）
//	Validate() 只会在**安全相关**字段缺失时 Fatal（如 Auth.Enabled=true 但
//	JWTSecret 为空）。
//
// 【为什么 AuthConfig.TokenExpiry/RefreshExpiry 是 string】
//
//	历史遗留——应该改成 time.Duration。目前值在 main.go 里被硬编码覆盖
//	（24h / 168h）。属于已知待修。
//
// ============================================================================
package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config is the root configuration structure for the entire agent system.
type Config struct {
	Server     ServerConfig     `mapstructure:"server"`
	LLM        LLMConfig        `mapstructure:"llm"`
	Redis      RedisConfig      `mapstructure:"redis"`
	Postgres   PostgresConfig   `mapstructure:"postgres"`
	Qdrant     QdrantConfig     `mapstructure:"qdrant"`
	Temporal   TemporalConfig   `mapstructure:"temporal"`
	Sandbox    SandboxConfig    `mapstructure:"sandbox"`
	MCP        MCPConfig        `mapstructure:"mcp"`
	RAG        RAGConfig        `mapstructure:"rag"`
	Session    SessionConfig    `mapstructure:"session"`
	Security   SecurityConfig   `mapstructure:"security"`
	Logging    LoggingConfig    `mapstructure:"logging"`
	Auth       AuthConfig       `mapstructure:"auth"`
	Tracing    TracingConfig    `mapstructure:"tracing"`
	PTY        PTYConfig        `mapstructure:"pty"`
	TreeSitter TreeSitterConfig `mapstructure:"tree_sitter"`
	LSP        LSPConfig        `mapstructure:"lsp"`
}

// ServerConfig holds HTTP/WS server settings.
type ServerConfig struct {
	HTTPAddr        string        `mapstructure:"http_addr"`
	WSAddr          string        `mapstructure:"ws_addr"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

// LLMProviderConfig defines settings for a single LLM provider.
type LLMProviderConfig struct {
	Provider           string        `mapstructure:"provider"`
	APIKey             string        `mapstructure:"api_key"`
	Model              string        `mapstructure:"model"`
	BaseURL            string        `mapstructure:"base_url"`
	MaxTokens          int           `mapstructure:"max_tokens"`
	Temperature        float32       `mapstructure:"temperature"`
	Timeout            time.Duration `mapstructure:"timeout"`
	ContextWindow      int           `mapstructure:"context_window"`       // Model's context window size (0 = auto-detect from model name)
	EnablePromptCaching bool         `mapstructure:"enable_prompt_caching"` // Send cache_control markers to Anthropic-compatible providers
}

// CircuitBreakerConfig defines circuit breaker parameters.
type CircuitBreakerConfig struct {
	MaxFailures     int           `mapstructure:"max_failures"`
	Timeout         time.Duration `mapstructure:"timeout"`
	HalfOpenMaxReqs int           `mapstructure:"half_open_max_requests"`
}

// LLMConfig holds all LLM-related configuration.
type LLMConfig struct {
	Primary        LLMProviderConfig    `mapstructure:"primary"`
	Fallback       LLMProviderConfig    `mapstructure:"fallback"`
	CircuitBreaker CircuitBreakerConfig `mapstructure:"circuit_breaker"`
	Router         RouterConfig         `mapstructure:"router"`
}

// RouterConfig configures intent-based model tier routing. When all model
// fields are empty the router is treated as disabled (orchestrator skips
// ApplyRoute entirely — full back-compat). Set heavy/medium/light to enable
// adaptive selection: see internal/llm/router.go for the rule table.
//
// Mirrors llm.RouterConfig to avoid a config → llm import cycle (llm already
// imports config); main.go converts between the two at wire-up.
type RouterConfig struct {
	HeavyModel  string `mapstructure:"heavy_model"`
	MediumModel string `mapstructure:"medium_model"`
	LightModel  string `mapstructure:"light_model"`

	HeavyMaxTokens  int `mapstructure:"heavy_max_tokens"`
	MediumMaxTokens int `mapstructure:"medium_max_tokens"`
	LightMaxTokens  int `mapstructure:"light_max_tokens"`
}

// Enabled reports whether at least one model tier is configured.
func (r RouterConfig) Enabled() bool {
	return r.HeavyModel != "" || r.MediumModel != "" || r.LightModel != ""
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Addr         string `mapstructure:"addr"`
	Password     string `mapstructure:"password"`
	DB           int    `mapstructure:"db"`
	PoolSize     int    `mapstructure:"pool_size"`
	MinIdleConns int    `mapstructure:"min_idle_conns"`
}

// PostgresConfig holds PostgreSQL connection settings.
type PostgresConfig struct {
	DSN             string        `mapstructure:"dsn"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
}

// QdrantConfig holds Qdrant vector database settings.
type QdrantConfig struct {
	Addr       string        `mapstructure:"addr"`
	Collection string        `mapstructure:"collection"`
	VectorSize int           `mapstructure:"vector_size"`
	Timeout    time.Duration `mapstructure:"timeout"`
}

// TemporalConfig holds Temporal workflow engine settings.
type TemporalConfig struct {
	Host            string        `mapstructure:"host"`
	Namespace       string        `mapstructure:"namespace"`
	TaskQueue       string        `mapstructure:"task_queue"`
	WorkflowTimeout time.Duration `mapstructure:"workflow_timeout"`
	ActivityTimeout time.Duration `mapstructure:"activity_timeout"`
}

// SandboxConfig holds Docker sandbox execution settings.
type SandboxConfig struct {
	DockerHost   string            `mapstructure:"docker_host"`
	DefaultImage string            `mapstructure:"default_image"`
	Images       map[string]string `mapstructure:"images"`
	MemoryLimit  string            `mapstructure:"memory_limit"`
	CPULimit     string            `mapstructure:"cpu_limit"`
	Timeout      time.Duration     `mapstructure:"timeout"`
	NetworkMode  string            `mapstructure:"network_mode"`
	WorkspaceDir string            `mapstructure:"workspace_dir"`
}

// MCPServerConfig defines a single MCP server endpoint.
//
// 连接池优化字段（Optional, 默认 0 → fallback 到 legacy 单连接模式）：
//
//	PoolSize — 单逻辑 server 背后同时启动的 MCP 子进程数量。
//	           当 >=2 时，Gateway 自动做 "least-pending" 负载均衡，
//	           消除单子进程 stdin 序列化的吞吐瓶颈。
//	           典型值 3~4（受 MCP server 资源占用限制）。
//
//	Streaming — 是否向 LLM 下游吐 chunked 流（通过 MCP notifications/progress）。
//	            启用后 Orchestrator 可用 Gateway.CallToolStream 拿到 <-chan ToolChunk，
//	            LLM 响应可边收边推到 SSE；关闭时退化为 CallTool 单次阻塞返回。
type MCPServerConfig struct {
	Name      string            `mapstructure:"name"`
	Transport string            `mapstructure:"transport"` // "stdio" or "sse"
	Command   string            `mapstructure:"command"`
	Args      []string          `mapstructure:"args"`
	URL       string            `mapstructure:"url"` // for SSE transport
	Env       map[string]string `mapstructure:"env"`
	PoolSize  int               `mapstructure:"pool_size"` // ≥2 启用多子进程池
	Streaming bool              `mapstructure:"streaming"` // 启用 progress chunk 流
}

// MCPConfig holds MCP gateway configuration.
type MCPConfig struct {
	Servers []MCPServerConfig `mapstructure:"servers"`
}

// RAGConfig holds RAG engine configuration including embedding API credentials.
// The embedding service requires its own base_url and api_key because embedding
// models (e.g., text-embedding-3-small) use the OpenAI API format, which may
// differ from the primary LLM provider (e.g., Anthropic Claude).
type RAGConfig struct {
	ChunkMaxTokens int    `mapstructure:"chunk_max_tokens"`
	OverlapTokens  int    `mapstructure:"overlap_tokens"`
	EmbeddingModel string `mapstructure:"embedding_model"`

	// EmbeddingProvider selects the embedding backend:
	//   "openai" - use OpenAI-compatible API (requires embedding_base_url + api_key)
	//   "local"  - use local hash-based embedder (no external dependency, lower quality)
	//   ""       - auto: try openai first, fall back to local if credentials missing
	EmbeddingProvider string `mapstructure:"embedding_provider"`

	// Embedding API credentials (OpenAI-compatible endpoint)
	EmbeddingBaseURL string `mapstructure:"embedding_base_url"` // e.g., "https://api.openai.com/v1" or compatible proxy
	EmbeddingAPIKey  string `mapstructure:"embedding_api_key"`  // API key for the embedding endpoint

	// Reranking (requires a cross-encoder model endpoint)
	RerankEnabled bool   `mapstructure:"rerank_enabled"`
	RerankModel   string `mapstructure:"rerank_model"`    // e.g., "BAAI/bge-reranker-v2-m3"
	RerankBaseURL string `mapstructure:"rerank_base_url"` // Reranker API endpoint
	RerankAPIKey  string `mapstructure:"rerank_api_key"`  // Reranker API key
	TopK          int    `mapstructure:"top_k"`
	RerankTopN    int    `mapstructure:"rerank_top_n"`

	// File watcher for incremental indexing
	WatchEnabled bool   `mapstructure:"watch_enabled"` // Enable file watcher for auto-reindexing
	WatchPath    string `mapstructure:"watch_path"`    // Path to watch (defaults to current repo)

	// Embedding cache mode:
	//   "memory" - in-memory LRU only (default, no Redis dependency)
	//   "redis"  - Redis-backed cache (persistent, cross-instance)
	//   "tiered" - L1 memory + L2 Redis (best performance + persistence)
	//   ""       - disabled (no caching, always call embedding API)
	EmbeddingCacheMode string `mapstructure:"embedding_cache_mode"`

	// Query rewrite mode:
	//   "none"   - no rewriting (default)
	//   "hyde"   - HyDE (Hypothetical Document Embeddings) via LLM
	//   "expand" - keyword expansion (camelCase/snake_case variants)
	QueryRewriteMode string `mapstructure:"query_rewrite_mode"`
}

// SessionConfig holds session management settings.
type SessionConfig struct {
	MaxHistoryTokens       int           `mapstructure:"max_history_tokens"`
	SummaryThresholdTokens int           `mapstructure:"summary_threshold_tokens"`
	TTL                    time.Duration `mapstructure:"ttl"`
	CompactionMode         string        `mapstructure:"compaction_mode"` // "truncate" (default) or "summarize"
}

// SecurityConfig holds security rule settings.
type SecurityConfig struct {
	SensitivePatterns       []string      `mapstructure:"sensitive_patterns"`
	RequireApprovalCommands []string      `mapstructure:"require_approval_commands"`
	EgressEnabled           bool          `mapstructure:"egress_enabled"`
	EgressAllowedHosts      []string      `mapstructure:"egress_allowed_hosts"`
	ToolApprovalTimeout     time.Duration `mapstructure:"tool_approval_timeout"` // 0 = use default (5 min)
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Output string `mapstructure:"output"`
}

// AuthConfig holds authentication and authorization settings.
type AuthConfig struct {
	Enabled       bool   `mapstructure:"enabled"`
	JWTSecret     string `mapstructure:"jwt_secret"`
	JWTIssuer     string `mapstructure:"jwt_issuer"`
	TokenExpiry   string `mapstructure:"token_expiry"`
	RefreshExpiry string `mapstructure:"refresh_expiry"`
}

// TracingConfig holds OpenTelemetry tracing settings.
type TracingConfig struct {
	Enabled     bool    `mapstructure:"enabled"`
	Endpoint    string  `mapstructure:"endpoint"`
	ServiceName string  `mapstructure:"service_name"`
	SampleRate  float64 `mapstructure:"sample_rate"`
	Insecure    bool    `mapstructure:"insecure"`
}

// PTYConfig holds persistent PTY session settings.
type PTYConfig struct {
	Enabled                bool   `mapstructure:"enabled"`
	Backend                string `mapstructure:"backend"` // "docker" or "local"
	Image                  string `mapstructure:"image"`
	MaxSessionsPerWorkspace int   `mapstructure:"max_sessions_per_workspace"`
	IdleTimeout            string `mapstructure:"idle_timeout"`
	CommandTimeout         string `mapstructure:"command_timeout"`
	MemoryLimit            int64  `mapstructure:"memory_limit"`
	CPUQuota               int64  `mapstructure:"cpu_quota"`
	OutputLimit            int    `mapstructure:"output_limit"`
	Shell                  string `mapstructure:"shell"`
}

// TreeSitterConfig holds tree-sitter AST parser settings.
type TreeSitterConfig struct {
	Enabled     bool     `mapstructure:"enabled"`
	Languages   []string `mapstructure:"languages"`
	MaxFileSize int      `mapstructure:"max_file_size"`
}

// LSPServerConfig defines a single LSP server.
type LSPServerConfig struct {
	Command   string   `mapstructure:"command"`
	Args      []string `mapstructure:"args"`
	Languages []string `mapstructure:"languages"`
}

// LSPConfig holds LSP client settings.
type LSPConfig struct {
	Enabled               bool                       `mapstructure:"enabled"`
	Servers               map[string]LSPServerConfig `mapstructure:"servers"`
	InitializationTimeout string                     `mapstructure:"initialization_timeout"`
	RequestTimeout        string                     `mapstructure:"request_timeout"`
	MaxConcurrentRequests int                        `mapstructure:"max_concurrent_requests"`
}

// Load reads and parses the configuration from the given path.
// It supports environment variable overrides with the CODE_AGENT_ prefix.
func Load(configPath string) (*Config, error) {
	v := viper.New()

	// Set defaults
	v.SetDefault("server.http_addr", ":8080")
	v.SetDefault("server.ws_addr", ":8081")
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "60s")
	v.SetDefault("server.shutdown_timeout", "15s")
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")

	// Read config file
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		v.AddConfigPath("/etc/code-agent/configs")
	}

	// Environment variable override
	v.SetEnvPrefix("CODE_AGENT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Recursively walk the config tree and expand ${VAR} in every string
	// field — replaces the prior 18-line manual list that silently dropped
	// any field a contributor forgot to add (we hit this once with
	// RAG.EmbeddingBaseURL: the embedder POSTed to the literal unexpanded
	// URL and the index failed for every file). Opt-out per field via
	// `env_expand:"false"` struct tag.
	walkExpandEnv(reflect.ValueOf(&cfg).Elem())

	return &cfg, nil
}

// walkExpandEnv recursively applies expandEnv to every string field of v.
// It descends into struct values, slices of structs, and string maps, and
// honours the `env_expand:"false"` struct tag as an opt-out so a field that
// happens to contain a literal "${...}" stays unexpanded.
//
// Implementation notes:
//   - Only handles the kinds the config tree actually uses (struct, slice,
//     map[string]string, string). Anything else is left alone.
//   - Maps are mutated via SetMapIndex because map values are not
//     addressable in reflect.
//   - Slices of strings get each element expanded; slices of struct elements
//     are recursed into per-index so MCP.Servers[i].Args[] works.
func walkExpandEnv(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			if tag := field.Tag.Get("env_expand"); tag == "false" {
				continue
			}
			walkExpandEnv(v.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			walkExpandEnv(v.Index(i))
		}
	case reflect.Map:
		// Only handle string-keyed string maps (e.g. MCPServerConfig.Env).
		// Other map kinds are not used in the config tree.
		if v.Type().Key().Kind() == reflect.String && v.Type().Elem().Kind() == reflect.String {
			for _, key := range v.MapKeys() {
				val := v.MapIndex(key).String()
				v.SetMapIndex(key, reflect.ValueOf(expandEnv(val)))
			}
		}
	case reflect.Ptr:
		if !v.IsNil() {
			walkExpandEnv(v.Elem())
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(expandEnv(v.String()))
		}
	default:
		// Other kinds (bool, int, float, etc.) are not expansion targets.
	}
}

// expandEnv expands ${VAR} patterns via os.ExpandEnv. When the referenced
// env var is unset, os.ExpandEnv returns an empty string — we preserve
// that behaviour explicitly so downstream "fallback when empty" logic
// (e.g. RAG embedder falling back to LLM primary credentials) engages as
// intended. Strings without a ${ are passed through unchanged.
func expandEnv(s string) string {
	if strings.Contains(s, "${") {
		return os.ExpandEnv(s)
	}
	return s
}
