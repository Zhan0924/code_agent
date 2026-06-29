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
	Workspace  WorkspaceConfig  `mapstructure:"workspace"`
	Memory     MemoryConfig     `mapstructure:"memory"`
}

// MemoryConfig 控制长期记忆 (internal/memory) 的运行时阈值。所有字段都是
// optional —— 留空时走 NewExtractor / NewConflictResolver / NewRedisHot
// 等构造器的代码内默认值，保持开箱即用。生产部署可以按需调高/降低。
type MemoryConfig struct {
	// HotTTL 是 Redis 热层每条记忆的 TTL。默认 24h；如果工作负载里
	// 每日活跃 user 数较少（例如内部工具）可以调到 72h 提升热命中率。
	HotTTL time.Duration `mapstructure:"hot_ttl"`

	// HotScanLimit 是 RedisHot 单次 SCAN 命中 key 的上限（P1 #10）。
	// 默认 200，clamp [50, 2000]：50 = 文档化"≤ 50 entries per
	// (user, project)"的下限保护；2000 ≈ 20 个 SCAN batch + 20k
	// unmarshal，单调用 ~30ms 上限。
	//
	// 调用方（HybridStore.RetrieveByType 等）传入的 `limit` 可以临时
	// 把扫描窗口拉到 maxHotScanLimit；但稳态扫描预算由这个配置决定。
	// 大 tenant 看到 memory_hot_scan_truncated_total 持续 > 0 时应调大。
	// 0 = 使用代码默认 200。
	HotScanLimit int `mapstructure:"hot_scan_limit"`

	// ConflictThreshold 是 ConflictResolver.FindConflicts 的余弦相似度阈值。
	// 不同 embedding 模型最优值差异极大：OpenAI text-embedding-3 在 0.7+
	// 已可视作同义，bge-large 需要 0.85+。默认 0.85。
	ConflictThreshold float64 `mapstructure:"conflict_threshold"`

	// ConflictMargin 是 score-aware 合并里"新记忆要 override 老记忆所需
	// 的分数差"。默认 0.2。
	ConflictMargin float64 `mapstructure:"conflict_margin"`

	// PreserveHighScore: 是否保留高分老记忆（true = 不被低分新记忆覆盖）。
	// 默认 true。
	PreserveHighScore bool `mapstructure:"preserve_high_score"`

	// MaxConflictsToDedup 是 HybridStore.Store 单次最多消除的副本数（P1 #7）。
	// 默认 32 — 防止候选集异常雪崩（一次 DELETE 几百行会拖慢事务）。
	// 设为 0 退回默认值；设为 1 等效于禁用 dedup（仍合并 [0]，但不删其余）。
	MaxConflictsToDedup int `mapstructure:"max_conflicts_to_dedup"`

	// DuplicateThreshold 是 Extractor.isDuplicate 的 n-gram Jaccard 阈值。
	// 默认 0.7；放低会更激进地去重，放高更宽松。
	DuplicateThreshold float64 `mapstructure:"duplicate_threshold"`

	// MaxPerRun 是单次 ExtractFromInteraction 接受的最多 candidate 数。
	// 默认 10；LLM 偶尔会越界生成几十条，这是硬截断保护。
	MaxPerRun int `mapstructure:"max_per_run"`

	// DedupCandidateLimit 是 P1 #9 引入的 Extractor.isDuplicate 近邻
	// 查询 K：每次写入前从 hot+cold 拉 K 条候选做 cosine ≥ 0.85 判断。
	// 旧实现写死 5 → 大库 rank-6..30 的真重复被漏过。默认 30，clamp
	// [5, 200]；超出区间会在 Extractor.SetDedupCandidateLimit 中自动钳制。
	// 该路径不会触发 Touch / Promote，故调大不会影响 Decay 公平性。
	DedupCandidateLimit int `mapstructure:"dedup_candidate_limit"`

	// EmbeddingDim 是 pgvector 列的维度。0 = 使用 1536（text-embedding-3-small）。
	// 切换到其他 embedding 模型时必须同时迁移 PG schema。
	EmbeddingDim int `mapstructure:"embedding_dim"`

	// Decay 控制周期性遗忘任务的运行参数。
	Decay MemoryDecayConfig `mapstructure:"decay"`

	// Distill 控制 episodic→semantic 的周期性蒸馏任务。
	Distill MemoryDistillConfig `mapstructure:"distill"`

	// Access 控制召回路径的 access_count / last_accessed_at 批量回写。
	// 关闭后 Decay 会回到"读不推进 last_accessed_at"的不公平状态。
	Access MemoryAccessConfig `mapstructure:"access"`

	// Promote 控制召回路径的 cold→hot 异步回填（P1 #8）。关闭后
	// "老但常用"的高分 memory 永远进不了 hot tier，所有 cold 命中都
	// 走慢路径 pgvector。
	Promote MemoryPromoteConfig `mapstructure:"promote"`

	// Demote 控制 Decay 路径的 hot 主动驱逐（P1 #8）。关闭后 hot 副本
	// 即使衰减到 0.05 分仍占用 hot 24h 直到 TTL，污染 tier-1 召回。
	Demote MemoryDemoteConfig `mapstructure:"demote"`

	// EpisodicGC 控制周期性清理过期的 episodic memory。
	EpisodicGC MemoryEpisodicGCConfig `mapstructure:"episodic_gc"`
}

// MemoryEpisodicGCConfig configures the periodic deletion of old episodic memories
// to prevent infinite bloat when Distiller is disabled or tenant lacks enough episodes.
type MemoryEpisodicGCConfig struct {
	Enabled   bool          `mapstructure:"enabled"`
	Interval  time.Duration `mapstructure:"interval"`
	OlderThan time.Duration `mapstructure:"older_than"`
}

// MemoryPromoteConfig 配置 cold→hot 异步回填批处理器。
//
// 读路径在 Retrieve / RetrieveByType 末尾扫描融合结果，识别"cold-only
// 命中 + score ≥ Threshold"的条目，非阻塞入队由后台 goroutine 批 SET
// 到 hot。后续相同 query 即可命中 5ms 路径。
type MemoryPromoteConfig struct {
	// Enabled 是否启用 Promote 批处理器；默认 true。设为 false 会让
	// 高分老 memory 永远走 cold 慢路径。
	Enabled bool `mapstructure:"enabled"`
	// Threshold 只 Promote score ≥ 此值的条目。默认 0.7 —— 经验上
	// 该值已是高价值 memory，低于此值的偶发命中不值得占用 hot 24h TTL。
	Threshold float64 `mapstructure:"threshold"`
	// BatchSize 单次 Pipeline SET 的最大条目数。默认 50。
	BatchSize int `mapstructure:"batch_size"`
	// FlushInterval 时间触发的 batch 间隔。默认 5s。
	FlushInterval time.Duration `mapstructure:"flush_interval"`
	// QueueSize Promote chan 容量；满则 drop + metric。默认 256。
	QueueSize int `mapstructure:"queue_size"`
}

// MemoryDemoteConfig 配置 Decay 路径的 hot 主动驱逐。
//
// Decay 计算 newScore = m.Score * factor 之后，若 newScore < Threshold
// 且 m.Score ≥ Threshold（"穿越阈值"），即从 hot DEL（cold 保留）。这
// 防止低质量 memory 占用 hot 24h TTL 污染 tier-1 召回。
type MemoryDemoteConfig struct {
	// Enabled 显式开关。设为 false 时 Threshold=0 透传，禁用 demote 分支。
	Enabled bool `mapstructure:"enabled"`
	// Threshold 跌破此值的 memory 从 hot DEL。必须 > 0.01（score floor）。
	// 默认 0.3 —— 经验上 0.3 已是低信号区间。
	Threshold float64 `mapstructure:"threshold"`
}

// MemoryAccessConfig 配置召回路径的异步 Touch batcher。
//
// 读路径返回前把命中的 memory.ID 入队，由内部 goroutine 防抖批量
// UPDATE access_count + last_accessed_at。写放大上限 = 1 QPS per
// HybridStore 实例（每 FlushInterval 一次），与读 QPS 完全脱钩。
type MemoryAccessConfig struct {
	// Enabled 是否启用批量 Touch；默认 true。设为 false 会让 Decay 失
	// 去对"读"信号的感知 — 仅在 cold-tier 写性能极其紧张时考虑关闭。
	Enabled bool `mapstructure:"enabled"`
	// BatchSize 单次 UPDATE 处理的 ID 上限。默认 100。
	BatchSize int `mapstructure:"batch_size"`
	// FlushInterval 时间触发的 batch 间隔。默认 5s。
	FlushInterval time.Duration `mapstructure:"flush_interval"`
	// QueueSize touch chan 的容量；满则 drop + metric 报警。默认 1024。
	// 200 QPS Retrieve × 5 results = 1000 IDs/s 稳态下 1024 刚好够。
	QueueSize int `mapstructure:"queue_size"`
}

// MemoryDistillConfig 配置 internal/memory.Distiller 的周期触发。
// 与 Decay 一样默认禁用，避免空集群在没有 LLM 配额时被自动调用浪费 token。
type MemoryDistillConfig struct {
	// Enabled 显式开关；只有 true 时 main.go 才会启动 ticker。
	Enabled bool `mapstructure:"enabled"`
	// Interval 是两次蒸馏之间的间隔。默认 6h（一天 4 次）。
	Interval time.Duration `mapstructure:"interval"`
	// MaxEpisodicPerRun 是单次蒸馏从 episodic 池抽取的上限。默认 50。
	MaxEpisodicPerRun int `mapstructure:"max_episodic_per_run"`
	// MinEpisodicToTrigger 是触发蒸馏的最少 episodic 数。默认 3。低于
	// 此值跳过 LLM 调用——少量样本只会产生噪声，不会产生 insight。
	MinEpisodicToTrigger int `mapstructure:"min_episodic_to_trigger"`
	// Targets 是要扫描的 (user_id, project_id) 列表，作为 forced
	// inclusion：每个 tick 一定会蒸馏，即使 episodic 还没积累到
	// MinEpisodicForDiscovery（Distiller 自己仍会按 MinEpisodicToTrigger
	// 跳过空 tenant）。
	//
	// 单独使用 Targets 会让多租户部署不可扩展（每加一个 user/project
	// 都要改 yaml）。生产应让 AutoDiscover=true 接管发现路径，Targets
	// 仅留给"哪怕没数据也想跑一轮"的运维兜底。
	Targets []MemoryDistillTarget `mapstructure:"targets"`

	// AutoDiscover 控制 ticker 是否从 PG 自动发现需要蒸馏的活跃 tenant。
	// 默认 true（与 Enabled=true 绑定）—— 没有它，新增 tenant 在 yaml
	// reload 前永远拿不到蒸馏。设为 false 退回旧的"只扫 Targets"行为。
	AutoDiscover bool `mapstructure:"auto_discover"`

	// MaxTenantsPerTick 单 tick 处理的 tenant 上限（包括 AutoDiscover
	// + Targets 合并去重后的总数）。默认 32：6h 一次 × 32 个 tenant ×
	// 每 tenant 1 LLM call = 每天 128 LLM calls 的上限，对配额敏感的
	// 部署可下调。
	MaxTenantsPerTick int `mapstructure:"max_tenants_per_tick"`

	// MinEpisodicForDiscovery 是 AutoDiscover 的活跃度阈值：只有
	// undistilled episodic >= 此值的 tenant 才会被发现。默认 0 时
	// 取 MinEpisodicToTrigger（避免发现到 tenant 又被 Distiller 立即
	// skip 的浪费）。
	MinEpisodicForDiscovery int `mapstructure:"min_episodic_for_discovery"`
}

// MemoryDistillTarget identifies a single (user, project) pair the
// Distiller ticker should consolidate.
type MemoryDistillTarget struct {
	UserID    string `mapstructure:"user_id"`
	ProjectID string `mapstructure:"project_id"`
}

// MemoryDecayConfig 配置定时衰减任务的触发节奏。零值禁用调度器（仅手动
// 调用 HybridStore.Decay 时才会衰减）。
type MemoryDecayConfig struct {
	// Enabled 显式开关；只有 true 时 main.go 才会启动 ticker。
	Enabled bool `mapstructure:"enabled"`
	// Interval 是两次衰减扫描之间的间隔。默认 24h（每天一次）。
	Interval time.Duration `mapstructure:"interval"`
	// OlderThan 是 "多久没访问的记忆才参与衰减" 的阈值。默认 30 天。
	OlderThan time.Duration `mapstructure:"older_than"`
	// Factor 是衰减系数（每轮乘以这个数）。默认 0.95。
	Factor float64 `mapstructure:"factor"`
}

// WorkspaceConfig 控制 host workspace 上 run_workspace_cmd 的执行行为。
// 注意:与 SandboxConfig 不同——后者是 Docker 容器内的命令执行,这里是
// host 进程直接 exec 在 /tmp/agent-workspaces/<id> 下,LLM 生成的 go test /
// pytest / npm test 等走这条路径,无网络隔离仅有命令白名单 + 进程组超时。
type WorkspaceConfig struct {
	// CmdTimeout 是 run_workspace_cmd 单次 exec 的硬上限。命令撞到这个
	// 时间会被 SIGKILL 整个进程组(file_tools.go:Cancel)。LLM 通过
	// tool args.timeout_seconds 可在 [0, CmdTimeout] 范围内自定义更短
	// 的上限;不指定或 0 时按 CmdTimeout 兜底。零值时取 5 分钟默认。
	CmdTimeout time.Duration `mapstructure:"cmd_timeout"`
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

	// EmbeddingDimension specifies the output vector dimension for the embedding model.
	// DashScope text-embedding-v3 supports: 64, 128, 256, 512, 768, 1024.
	// Must match qdrant.vector_size. Defaults to 1024 if unset.
	EmbeddingDimension int `mapstructure:"embedding_dimension"`

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

	// AUDIT-P0-2: distillation + episodic GC default ON. Operators who
	// truly want the half-dead state (smoke tests, cost-sensitive CI)
	// must explicitly set memory.distill.enabled=false in YAML. Without
	// these defaults a config.yaml missing the section entirely (the
	// common case for new clusters) silently disables both schedulers.
	v.SetDefault("memory.distill.enabled", true)
	v.SetDefault("memory.distill.interval", "6h")
	v.SetDefault("memory.distill.max_episodic_per_run", 50)
	v.SetDefault("memory.distill.min_episodic_to_trigger", 3)
	v.SetDefault("memory.distill.auto_discover", true)
	v.SetDefault("memory.distill.max_tenants_per_tick", 32)
	v.SetDefault("memory.episodic_gc.enabled", true)
	v.SetDefault("memory.episodic_gc.interval", "24h")
	v.SetDefault("memory.episodic_gc.older_than", "720h")

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
