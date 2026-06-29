// Package main is the entry point for the Code Intelligence Agent service.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【职责】这是整个服务的唯一入口。它做且仅做三件事：
//
//  1. **读配置**：从 YAML + 环境变量加载配置，并做 schema 校验；
//  2. **装配依赖**：按拓扑顺序构造所有子系统，各子系统构造失败时决定
//     "fatal / warn 降级"两种处理方式；
//  3. **启动并监听退出信号**：起 HTTP server 和（可选）Temporal worker，
//     收到 SIGINT/SIGTERM 时有序关闭。
//
// 【"Redis 必需，其余全部可选" 的设计】
//
//	Redis 一挂，session 管理就瘫痪 → 直接 Fatal。
//	Qdrant/Docker/MCP/Postgres/Temporal 任何一个不可用，只 Warn 然后把对应
//	子系统设为 nil。handler 层必须做 nil 检查。好处：在开发机器/受限环境
//	仍能起最小可用的 Agent，不至于因为连不上 Qdrant 就完全跑不起来。
//
// 【"手动 DI 而非 fx/wire"】
//
//	总共就这一个入口，200 行把依赖拉齐。用框架反而增加认知负担。显式手写
//	DI 的好处是新人一眼能看出"谁依赖谁"。
//
// 【Setter 而不是构造参数】
//
//	orch.SetSkillRegistry / apiServer.SetMCPGateway 等 setter 的存在，是为
//	了**打破循环依赖**：Orchestrator 需要 Skill，但 Skill Registry 构造时
//	需要知道哪些 tool 可用（来自 Orchestrator 的 tools registry）。用 setter
//	把"构造阶段"和"接线阶段"分开。
//
// 【关闭顺序（defer 栈 LIFO）】
//
//	序：redis.Close → pgStore.Close → tracing.Shutdown → mcpGateway.Close
//	  → sandboxMgr.Close → qdrantStore.Close → logger.Sync
//	再加上 httpServer.Shutdown（从 signal 分支显式调用，不走 defer）。
//	顺序重要：依赖上层先关，底层最后关。
//
// ============================================================================
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/api"
	"github.com/agent/code_agent/internal/auth"
	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/generator"
	"github.com/agent/code_agent/internal/indexer"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/lsp"
	"github.com/agent/code_agent/internal/mcp"
	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/multiagent"
	"github.com/agent/code_agent/internal/orchestrator"
	"github.com/agent/code_agent/internal/planner"
	"github.com/agent/code_agent/internal/pty"
	"github.com/agent/code_agent/internal/rag"
	"github.com/agent/code_agent/internal/repomap"
	"github.com/agent/code_agent/internal/sandbox"
	"github.com/agent/code_agent/internal/security"
	"github.com/agent/code_agent/internal/session"
	"github.com/agent/code_agent/internal/skill"
	"github.com/agent/code_agent/internal/store"
	temporalpkg "github.com/agent/code_agent/internal/temporal"
	"github.com/agent/code_agent/internal/toollearn"
	"github.com/agent/code_agent/internal/tools"
	"github.com/agent/code_agent/internal/tracing"
	"github.com/agent/code_agent/internal/treesitter"
	"github.com/agent/code_agent/internal/workspace"
	"github.com/redis/go-redis/v9"
	temporalclient "go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"
	"go.uber.org/zap"
)

// Version / BuildTime 在构建时由 ldflags 注入。Makefile 中形如：
//
//	-ldflags "-X main.Version=$(git describe ...) -X main.BuildTime=$(date ...)"
//
// 便于 /metrics 和启动日志暴露构建信息，排查"到底在跑哪个版本"。
var (
	Version   = "dev"
	BuildTime = "unknown"
)

func main() {
	configPath := flag.String("config", "", "Path to configuration file")
	flag.Parse()

	// ─── Initialize Logger ───────────────────────────────────────────────
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("starting Code Intelligence Agent",
		zap.String("version", Version),
		zap.String("build_time", BuildTime),
	)

	// ─── Load Configuration ──────────────────────────────────────────────
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal("failed to load configuration", zap.Error(err))
	}
	if err := cfg.Validate(); err != nil {
		logger.Fatal("configuration validation failed", zap.Error(err))
	}
	logger.Info("configuration loaded and validated")

	// ─── Initialize Redis Client ─────────────────────────────────────────
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		PoolSize:     cfg.Redis.PoolSize,
		MinIdleConns: cfg.Redis.MinIdleConns,
	})

	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Fatal("failed to connect to Redis", zap.Error(err))
	}
	logger.Info("redis connected", zap.String("addr", cfg.Redis.Addr))

	// ─── Initialize Session Manager ──────────────────────────────────────
	sessionMgr := session.NewManager(rdb, &cfg.Session, logger)
	logger.Info("session manager initialized")

	// ─── Initialize LLM Client ───────────────────────────────────────────
	var egressHTTPClient *http.Client
	if cfg.Security.EgressEnabled {
		policy := &security.EgressPolicy{
			Enabled:       true,
			DefaultAction: "deny",
			AllowedHosts:  cfg.Security.EgressAllowedHosts,
			DNSAllowed:    true,
		}
		egressValidator, egressErr := security.NewEgressValidator(policy, logger)
		if egressErr != nil {
			logger.Fatal("failed to create egress validator", zap.Error(egressErr))
		}
		egressHTTPClient = security.NewEgressHTTPClient(egressValidator, cfg.LLM.Primary.Timeout)
		logger.Info("egress-protected HTTP client enabled for LLM",
			zap.Int("allowed_hosts", len(cfg.Security.EgressAllowedHosts)),
		)
	}
	llmClient, err := llm.NewClientWithOptions(&cfg.LLM, nil, egressHTTPClient, logger)
	if err != nil {
		logger.Fatal("failed to initialize LLM client", zap.Error(err))
	}
	logger.Info("LLM client initialized",
		zap.String("primary", cfg.LLM.Primary.Provider+"/"+cfg.LLM.Primary.Model),
	)

	// Optional intent-based model-tier router. Disabled unless at least one
	// of heavy/medium/light model is set in config — we don't want to silently
	// remap users' explicit primary/fallback model choice.
	var llmRouter *llm.Router
	if cfg.LLM.Router.Enabled() {
		llmRouter = llm.NewRouter(llm.RouterConfig{
			HeavyModel:      cfg.LLM.Router.HeavyModel,
			MediumModel:     cfg.LLM.Router.MediumModel,
			LightModel:      cfg.LLM.Router.LightModel,
			HeavyMaxTokens:  cfg.LLM.Router.HeavyMaxTokens,
			MediumMaxTokens: cfg.LLM.Router.MediumMaxTokens,
			LightMaxTokens:  cfg.LLM.Router.LightMaxTokens,
		}, logger)
		logger.Info("LLM model router enabled",
			zap.String("heavy", cfg.LLM.Router.HeavyModel),
			zap.String("medium", cfg.LLM.Router.MediumModel),
			zap.String("light", cfg.LLM.Router.LightModel),
		)
	}

	// ─── Wire LLM Summarizer into Session Manager ───────────────────────
	sessionMgr.Summarizer = session.NewLLMSummarizer(
		func(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
			resp, err := llmClient.ChatCompletion(ctx, &llm.ChatRequest{
				Messages: []models.Message{
					{Role: models.RoleSystem, Content: systemPrompt},
					{Role: models.RoleUser, Content: userPrompt},
				},
				Temperature: 0.2,
			})
			if err != nil {
				return "", err
			}
			return resp.Content, nil
		},
		logger,
	)
	logger.Info("LLM summarizer wired into session manager")

	// ─── Initialize RAG Engine (optional - depends on Qdrant) ────────────
	// [OPT-2] Wire the OpenAI Embedder to prevent nil pointer on IndexCode/Retrieve.
	var ragEngine *rag.Engine
	var embedder rag.Embedder // Shared embedder for RAG and memory systems

	// Create embedder independently of Qdrant — memory system needs it even without RAG
	embeddingProvider := cfg.RAG.EmbeddingProvider
	if embeddingProvider == "" {
		if cfg.RAG.EmbeddingAPIKey != "" && cfg.RAG.EmbeddingBaseURL != "" {
			embeddingProvider = "openai"
		} else if cfg.LLM.Primary.APIKey != "" {
			embeddingProvider = "openai"
		} else {
			embeddingProvider = "local"
		}
		logger.Info("embedding provider auto-detected", zap.String("provider", embeddingProvider))
	}
	switch embeddingProvider {
	case "openai":
		embedder = rag.NewOpenAIEmbedder(&cfg.RAG, &cfg.LLM.Primary, egressHTTPClient, logger)
	case "local":
		embedder = rag.NewLocalHashEmbedder(cfg.Qdrant.VectorSize, logger)
	default:
		logger.Warn("unknown embedding_provider, using local", zap.String("provider", embeddingProvider))
		embedder = rag.NewLocalHashEmbedder(cfg.Qdrant.VectorSize, logger)
	}

	// Wrap embedder with cache if configured
	cacheMode := cfg.RAG.EmbeddingCacheMode
	if cacheMode == "" {
		cacheMode = "memory" // default to memory cache
	}
	namespace := cfg.RAG.EmbeddingModel
	if namespace == "" {
		namespace = "default"
	}

	switch cacheMode {
	case "memory":
		memCache := rag.NewMemoryEmbeddingCache(10000, namespace, logger)
		embedder = rag.NewCachedEmbedder(embedder, memCache, logger)
		logger.Info("embedding cache enabled", zap.String("mode", "memory"), zap.Int("capacity", 10000))
	case "redis":
		redisCache := rag.NewRedisEmbeddingCache(rdb, namespace, logger)
		embedder = rag.NewCachedEmbedder(embedder, redisCache, logger)
		logger.Info("embedding cache enabled", zap.String("mode", "redis"))
	case "tiered":
		memCache := rag.NewMemoryEmbeddingCache(10000, namespace, logger)
		redisCache := rag.NewRedisEmbeddingCache(rdb, namespace, logger)
		tieredCache := rag.NewTieredCache(memCache, redisCache, logger)
		embedder = rag.NewCachedEmbedder(embedder, tieredCache, logger)
		logger.Info("embedding cache enabled", zap.String("mode", "tiered"), zap.Int("l1_capacity", 10000))
	case "disabled", "":
		logger.Info("embedding cache disabled")
	default:
		logger.Warn("unknown embedding_cache_mode, cache disabled", zap.String("mode", cacheMode))
	}

	qdrantStore, err := rag.NewQdrantStore(&cfg.Qdrant, logger)
	if err != nil {
		logger.Warn("Qdrant not available, RAG disabled", zap.Error(err))
	} else {
		// Initialize reranker if configured
		var reranker rag.Reranker
		if cfg.RAG.RerankEnabled && cfg.RAG.RerankModel != "" && cfg.RAG.RerankBaseURL != "" {
			reranker = rag.NewAPIReranker(&cfg.RAG, egressHTTPClient, logger)
		}

		ragEngine = rag.NewEngine(embedder, qdrantStore, reranker, &cfg.RAG, logger)
		defer qdrantStore.Close()

		// Wire query rewriter if configured
		queryRewriteMode := cfg.RAG.QueryRewriteMode
		if queryRewriteMode != "" && queryRewriteMode != "none" {
			var rewriter rag.QueryRewriter
			switch queryRewriteMode {
			case "hyde":
				// Create LLM function for HyDE
				llmFunc := func(ctx context.Context, messages []models.Message) (string, error) {
					resp, err := llmClient.ChatCompletion(ctx, &llm.ChatRequest{
						Messages:    messages,
						MaxTokens:   500,
						Temperature: 0.7,
					})
					if err != nil {
						return "", err
					}
					return resp.Content, nil
				}
				rewriter = rag.NewHyDERewriter(llmFunc, logger)
			case "expand":
				rewriter = rag.NewKeywordExpander(logger)
			default:
				logger.Warn("unknown query_rewrite_mode, rewriter disabled", zap.String("mode", queryRewriteMode))
			}
			if rewriter != nil {
				ragEngine.SetQueryRewriter(rewriter)
				logger.Info("query rewriter enabled", zap.String("mode", queryRewriteMode))
			}
		}

		logger.Info("RAG engine initialized",
			zap.String("collection", cfg.Qdrant.Collection),
			zap.String("embedding_provider", embeddingProvider),
			zap.String("embedding_model", cfg.RAG.EmbeddingModel),
		)
	}

	// ─── Initialize Docker Sandbox (optional - depends on Docker) ────────
	var sandboxMgr *sandbox.Manager
	sandboxMgr, err = sandbox.NewManager(&cfg.Sandbox, logger)
	if err != nil {
		logger.Warn("Docker sandbox not available", zap.Error(err))
		sandboxMgr = nil
	} else {
		defer sandboxMgr.Close()
		logger.Info("sandbox manager initialized")
	}

	// ─── Initialize MCP Gateway ──────────────────────────────────────────
	// SSE transports require an *http.Client; when egress enforcement is
	// disabled, egressHTTPClient is nil and SSE configs would fail to dial.
	// Fall back to the default client so MCP-over-SSE works regardless of
	// the security toggle — egress-on still uses the wrapped client which
	// enforces the ACL.
	mcpHTTPClient := egressHTTPClient
	if mcpHTTPClient == nil {
		mcpHTTPClient = http.DefaultClient
	}
	mcpGateway, err := mcp.NewGateway(&cfg.MCP, mcpHTTPClient, logger)
	if err != nil {
		logger.Warn("MCP gateway initialization failed", zap.Error(err))
	} else {
		defer mcpGateway.Close()
		mcpGateway.StartHealthCheck(30 * time.Second)
		logger.Info("MCP gateway initialized",
			zap.Int("servers", len(cfg.MCP.Servers)),
		)
	}

	// ─── Initialize PostgreSQL Store (optional) ──────────────────────────
	var pgStore *store.Store
	if cfg.Postgres.DSN != "" {
		pgCfg := &store.PostgresConfig{
			Host: "localhost", Port: 5432, User: "agent",
			Password: "agent_secret", Database: "code_agent", SSLMode: "disable",
			MaxOpenConns: cfg.Postgres.MaxOpenConns,
			MaxIdleConns: cfg.Postgres.MaxIdleConns,
		}
		// If DSN is provided, use it directly via store.NewStoreFromDSN
		pgStore, err = store.NewStoreFromDSN(cfg.Postgres.DSN, pgCfg.MaxOpenConns, pgCfg.MaxIdleConns, logger)
		if err != nil {
			logger.Warn("PostgreSQL not available, persistence disabled", zap.Error(err))
		} else {
			if err := pgStore.Migrate(ctx); err != nil {
				logger.Error("database migration failed", zap.Error(err))
			} else {
				logger.Info("PostgreSQL store initialized and migrated")
			}
			defer pgStore.Close()
		}
	}

	// ─── Wire Session PG Long-Term Store ─────────────────────────────────
	// Without this, Redis hot/cold TTL (24h/48h) silently drops sessions from
	// the sidebar forever. With it, the PG `sessions` row is authoritative
	// and Redis becomes a cache. Graceful degradation: no PG → legacy
	// Redis-only behavior (start-time Warn keeps the limitation visible).
	if pgStore != nil {
		sessionMgr.PGStore = session.NewPGSessionStore(pgStore.DB(), logger)
		logger.Info("session PG long-term store enabled")
	} else {
		logger.Warn("session PG long-term store disabled (no Postgres DSN); sessions will be lost after Redis TTL")
	}

	// ─── Initialize Tracing (optional) ───────────────────────────────────
	tracingCfg := &tracing.Config{
		Enabled:     cfg.Tracing.Enabled,
		Endpoint:    cfg.Tracing.Endpoint,
		ServiceName: cfg.Tracing.ServiceName,
		SampleRate:  cfg.Tracing.SampleRate,
		Insecure:    cfg.Tracing.Insecure,
	}
	traceProvider, err := tracing.NewProvider(tracingCfg, logger)
	if err != nil {
		logger.Warn("tracing initialization failed", zap.Error(err))
	} else {
		defer traceProvider.Shutdown(context.Background())
	}

	// ─── Initialize Auth (optional) ──────────────────────────────────────
	var jwtMgr *auth.JWTManager
	var apiKeyStore *auth.APIKeyStore
	authEnabled := cfg.Auth.Enabled
	if authEnabled {
		tokenExpiry := 15 * time.Minute
		if cfg.Auth.TokenExpiry != "" {
			if d, err := time.ParseDuration(cfg.Auth.TokenExpiry); err == nil {
				tokenExpiry = d
			} else {
				logger.Warn("invalid token_expiry, using default 15m", zap.String("value", cfg.Auth.TokenExpiry), zap.Error(err))
			}
		}
		refreshExpiry := 7 * 24 * time.Hour
		if cfg.Auth.RefreshExpiry != "" {
			if d, err := time.ParseDuration(cfg.Auth.RefreshExpiry); err == nil {
				refreshExpiry = d
			} else {
				logger.Warn("invalid refresh_expiry, using default 7d", zap.String("value", cfg.Auth.RefreshExpiry), zap.Error(err))
			}
		}
		jwtCfg := &auth.JWTConfig{
			SecretKey:     cfg.Auth.JWTSecret,
			Issuer:        cfg.Auth.JWTIssuer,
			TokenExpiry:   tokenExpiry,
			RefreshExpiry: refreshExpiry,
		}
		if jwtCfg.SecretKey == "" {
			jwtCfg = auth.DefaultJWTConfig()
			logger.Warn("using auto-generated JWT secret (set CODE_AGENT_AUTH_JWT_SECRET for production)")
		}
		jwtMgr = auth.NewJWTManager(jwtCfg, logger)
		apiKeyStore = auth.NewAPIKeyStore()
		logger.Info("authentication enabled", zap.String("issuer", jwtCfg.Issuer))
	} else {
		logger.Warn("authentication DISABLED — all endpoints are public")
	}

	// ─── Initialize Orchestrator ─────────────────────────────────────────
	orch := orchestrator.NewOrchestrator(
		llmClient,
		sessionMgr,
		ragEngine,
		sandboxMgr,
		mcpGateway,
		&cfg.Security,
		logger,
		pgStore,
	)
	if cfg.Session.CompactionMode != "" {
		orch.SetCompactionMode(cfg.Session.CompactionMode)
	}
	if cfg.Workspace.CmdTimeout > 0 {
		orch.SetWorkspaceCmdTimeout(cfg.Workspace.CmdTimeout)
		logger.Info("workspace.cmd_timeout applied", zap.Duration("cmd_timeout", cfg.Workspace.CmdTimeout))
	}
	if llmRouter != nil {
		orch.SetRouter(llmRouter)
		logger.Info("LLM router wired into orchestrator")
	}
	// Stream event cache: mirrors ReactStreamEvent to Redis so the frontend
	// can replay+follow in-progress tasks after a page refresh. Without this
	// the SSE stream is in-memory only and a refresh loses everything.
	streamCache := orchestrator.NewStreamCache(rdb, logger)
	orch.SetStreamCache(streamCache)
	logger.Info("stream event cache wired into orchestrator")
	// 清扫上一进程 SIGTERM / 崩溃留下的孤儿 running 标记，避免前端 /resume 死等。
	if cleared, err := streamCache.ClearAllRunning(context.Background()); err != nil {
		logger.Warn("clear stale stream-running flags failed", zap.Error(err))
	} else if cleared > 0 {
		logger.Info("cleared stale stream-running flags from previous process",
			zap.Int("count", cleared))
	}
	// Wire Planner for complex task DAG execution
	plannerAdapter := orchestrator.NewLLMCallerAdapter(llmClient)
	p := planner.NewPlanner(plannerAdapter, logger)
	orch.AttachPlanner(p)
	logger.Info("orchestrator initialized (planner attached)")

	// Wire multi-agent Supervisor for parallel plan execution. Inject the
	// registry-backed file-write classifier so conflict detection uses the
	// same metadata source as the rest of the system (avoiding the latent
	// rename_symbol drift the hardcoded list previously had).
	supCfg := multiagent.DefaultSupervisorConfig()
	supervisor := multiagent.NewSupervisor(
		orch.NewToolDispatcherAdapter(), supCfg, logger,
		multiagent.WithFileWriteClassifier(orch.IsFileWriteTool),
	)
	orch.AttachSupervisor(supervisor)
	logger.Info("multi-agent supervisor attached")

	// ─── Wire Memory Store (optional) ───────────────────────────────────
	memAdapter := NewMemoryAdapter(rdb, pgStore, embedder, logger, cfg.Memory)
	if memAdapter != nil {
		orch.SetMemoryStore(memAdapter)
		logger.Info("long-term memory store wired into orchestrator")
	}

	// ─── Wire ToolLearn PG Store (optional) ─────────────────────────────
	// Persists tool-execution feedback so adaptive policy / distiller survive
	// restarts. Falls back to in-memory if Postgres is unavailable.
	if pgStore != nil {
		tlStore := toollearn.NewPGStore(pgStore.DB())
		if err := tlStore.Migrate(); err != nil {
			logger.Warn("toollearn pg migration failed, feedback stays in-memory", zap.Error(err))
		} else {
			orch.SetToolLearnStore(tlStore)
			logger.Info("toollearn PG store wired into orchestrator (tool_feedback table)")
		}
	}

	// ─── Wire Trajectory PG Store (optional) ────────────────────────────
	// Successful tool sequences are recalled into the prompt as a "what
	// worked last time" hint. Without PG, the default in-memory
	// TrajectoryMemory caps at 50 entries and resets every restart;
	// with PG + embedder, recall becomes KNN over intent embeddings so
	// "fix a flaky test" can match a previous "stabilize an integration
	// test" trajectory even though the intent strings differ.
	if pgStore != nil {
		// embedDim falls back to memCfg.EmbeddingDim when the operator
		// has already pinned a value for the memory subsystem; the
		// trajectory table reuses the same vector size to avoid running
		// two embedding models in parallel.
		dim := cfg.Memory.EmbeddingDim
		if dim <= 0 {
			dim = 1024
		}
		var ie agentloop.IntentEmbedder
		if embedder != nil {
			// memory.Embedder satisfies agentloop.IntentEmbedder
			// structurally — same Embed(ctx, []string) signature.
			ie = embedder
		}
		trajStore := agentloop.NewPGTrajectoryStore(pgStore.DB(), logger, ie, dim)
		if err := trajStore.Migrate(); err != nil {
			logger.Warn("trajectory pg migration failed; staying with in-memory store",
				zap.Error(err))
		} else {
			orch.SetTrajectoryStore(trajStore)
			logger.Info("trajectory PG store wired into orchestrator",
				zap.Bool("knn_enabled", ie != nil),
				zap.Int("embed_dim", dim))
		}
	}

	// memoryDecayStop / memoryDistillStop / memoryAccessStop, when
	// non-nil, can be called during shutdown to halt their respective
	// background loops cleanly. Declared up here so the Drain block
	// below can invoke them; nil-safe to keep main flow readable.
	var (
		memoryDecayStop   func()
		memoryDistillStop func()
		memoryEpisodicGCStop func()
		memoryAccessStop  func()
		memoryPromoteStop func()
	)

	// ─── Wire Memory Extractor (optional) ────────────────────────────────
	if memAdapter != nil {
		// Get the underlying HybridStore from adapter to create extractor.
		// We pass the embedder so isDuplicate() can use cosine similarity
		// instead of word-Jaccard (the latter collapses on Chinese text —
		// historical bug fix).
		memoryExtractor := memory.NewExtractor(memAdapter.HybridStore(), llmClient, logger)
		if embedder != nil {
			memoryExtractor.SetEmbedder(embedder)
		}
		if cfg.Memory.MaxPerRun > 0 {
			memoryExtractor.SetMaxPerRun(cfg.Memory.MaxPerRun)
		}
		if cfg.Memory.DedupCandidateLimit != 0 {
			memoryExtractor.SetDedupCandidateLimit(cfg.Memory.DedupCandidateLimit)
		}
		if cfg.Memory.DedupCandidateLimit > 0 {
			memoryExtractor.SetDedupCandidateLimit(cfg.Memory.DedupCandidateLimit)
		}
		orch.SetMemoryExtractor(memoryExtractor)
		logger.Info("memory extractor wired into orchestrator",
			zap.Bool("with_embedder_dedup", embedder != nil),
			zap.Int("max_per_run", cfg.Memory.MaxPerRun))

		// ─── Active Memory Tools (MemGPT-style core memory) ─────────────────
		// Wires *both* directions:
		//   1) Tools — core_memory_append / core_memory_replace let the LLM
		//      write into persona / human_context / project_context sections.
		//   2) Read path — orch.SetCoreMemory makes buildLongTermMemory
		//      inject those sections into every prompt's [Core Memory] block.
		// Without (2), the tools wrote to Redis but the LLM never saw it
		// back (the original "write-only blackhole" bug).
		// Setup Core Memory
		coreManager := memory.NewRedisCoreMemory(rdb, logger)
		if memAdapter != nil {
			memoryExtractor.WithCorePromoter(coreManager)
		}
		orch.SetCoreMemory(coreManager)
		memoryTools := tools.NewMemoryToolsProvider(coreManager)
		for _, tool := range memoryTools.Tools() {
			if err := orch.RegisterDynamicTool(tool); err != nil {
				logger.Error("failed to register memory tool", zap.Error(err))
			}
		}
		logger.Info("active memory tools registered (read+write path wired)")

		// ─── Periodic Memory Decay (optional) ───────────────────────────
		// Without a scheduler, the Decay path lives but is never invoked
		// (the "dead schedule" historical bug). Enable in config to apply
		// Ebbinghaus-style forgetting to long-idle memories.
		if cfg.Memory.Decay.Enabled {
			interval := cfg.Memory.Decay.Interval
			if interval <= 0 {
				interval = 24 * time.Hour
			}
			olderThan := cfg.Memory.Decay.OlderThan
			if olderThan <= 0 {
				olderThan = 30 * 24 * time.Hour
			}
			factor := cfg.Memory.Decay.Factor
			if factor <= 0 || factor >= 1 {
				factor = 0.95
			}
			decayCtx, cancelDecay := context.WithCancel(context.Background())
			memoryDecayStop = cancelDecay
			go runMemoryDecayLoop(decayCtx, memAdapter.HybridStore(), interval, olderThan, factor, logger)
			logger.Info("memory decay scheduler started",
				zap.Duration("interval", interval),
				zap.Duration("older_than", olderThan),
				zap.Float64("factor", factor))
		}

		// ─── Periodic Memory Distillation (optional) ────────────────────
		// Consolidates raw episodic traces into a smaller, higher-signal
		// semantic memory. Disabled by default — costs LLM tokens, so
		// production should opt in deliberately and configure Targets.
		if cfg.Memory.Distill.Enabled {
			distillCtx, cancelDistill := context.WithCancel(context.Background())
			memoryDistillStop = cancelDistill
			go runMemoryDistillLoop(
				distillCtx,
				memAdapter.HybridStore(),
				&distillerLLMAdapter{c: llmClient},
				memory.NewBlackboard(rdb, logger),
				cfg.Memory.Distill,
				logger,
			)
			logger.Info("memory distill scheduler started",
				zap.Duration("interval", cfg.Memory.Distill.Interval),
				zap.Int("targets", len(cfg.Memory.Distill.Targets)))
		}

		if cfg.Memory.EpisodicGC.Enabled {
			gcCtx, cancelGC := context.WithCancel(context.Background())
			memoryEpisodicGCStop = cancelGC
			go runEpisodicGCLoop(
				gcCtx,
				memAdapter.HybridStore(),
				cfg.Memory.EpisodicGC,
				logger,
			)
			logger.Info("episodic gc scheduler started",
				zap.Duration("interval", cfg.Memory.EpisodicGC.Interval),
				zap.Duration("older_than", cfg.Memory.EpisodicGC.OlderThan))
		}


		// ─── Async Access Batcher (read-path Touch) ─────────────────────
		// Without this, every Retrieve returned memories WITHOUT advancing
		// last_accessed_at, so Decay treated frequently-read entries the
		// same as never-read ones. Enabling the batcher closes that loop
		// at ≤ 1 UPDATE QPS per HybridStore replica (debounced).
		// Default-on; explicit `enabled: false` retains legacy behaviour.
		if cfg.Memory.Access.Enabled {
			memAdapter.HybridStore().EnableAccessBatcher(memory.AccessBatcherOptions{
				BatchSize:     cfg.Memory.Access.BatchSize,
				FlushInterval: cfg.Memory.Access.FlushInterval,
				QueueSize:     cfg.Memory.Access.QueueSize,
			})
			accessCtx, cancelAccess := context.WithCancel(context.Background())
			memoryAccessStop = cancelAccess
			go memAdapter.HybridStore().StartAccessBatcher(accessCtx)
			logger.Info("memory access batcher started")
		}

		// ─── Async Promote Batcher (read-path cold→hot back-fill) ───────
		// Without this, "old but frequently retrieved" memories never
		// reach the hot tier, so every cold-only hit pays the 50-200ms
		// pgvector cost instead of the 5ms hot path. P1 #8.
		if cfg.Memory.Promote.Enabled {
			memAdapter.HybridStore().EnablePromoteBatcher(memory.PromoteOptions{
				Threshold:     cfg.Memory.Promote.Threshold,
				BatchSize:     cfg.Memory.Promote.BatchSize,
				FlushInterval: cfg.Memory.Promote.FlushInterval,
				QueueSize:     cfg.Memory.Promote.QueueSize,
			})
			promoteCtx, cancelPromote := context.WithCancel(context.Background())
			memoryPromoteStop = cancelPromote
			go memAdapter.HybridStore().StartPromoteBatcher(promoteCtx)
			logger.Info("memory promote batcher started",
				zap.Float64("threshold", cfg.Memory.Promote.Threshold))
		}

		// ─── Demote Threshold (Decay-time hot eviction) ─────────────────
		// Stand-alone setting (no batcher) — applied inline by the Decay
		// path. P1 #8.
		if cfg.Memory.Demote.Enabled && cfg.Memory.Demote.Threshold > 0.01 {
			memAdapter.HybridStore().SetDemoteThreshold(cfg.Memory.Demote.Threshold)
			logger.Info("memory demote threshold configured",
				zap.Float64("threshold", cfg.Memory.Demote.Threshold))
		}
	}

	// ─── Initialize API Server ───────────────────────────────────────────
	var pgPingFn func(ctx context.Context) error
	if pgStore != nil {
		pgPingFn = pgStore.Ping
	}
	apiServer := api.NewServer(orch, sessionMgr, logger, api.ServerOptions{
		JWTManager:   jwtMgr,
		APIKeyStore:  apiKeyStore,
		AuthEnabled:  authEnabled,
		RedisClient:  rdb,
		PGHealthPing: pgPingFn,
	})

	// Wire long-term memory store for /api/v1/memory inspection endpoints.
	if memAdapter != nil {
		apiServer.SetMemoryStore(memAdapter.HybridStore())
		logger.Info("memory store wired into API server (/memory endpoints)")
	}

	// ─── Wire Indexer into API Server ────────────────────────────────────
	var repomapGen *repomap.Generator
	if ragEngine != nil {
		var idxOpts []indexer.IndexerOption
		if pgStore != nil {
			idxOpts = append(idxOpts, indexer.WithStore(pgStore))
		}
		idx := indexer.NewIndexer(ragEngine, &cfg.RAG, logger, idxOpts...)
		apiServer.SetIndexer(idx)
		logger.Info("indexer wired into API server")

		// ─── File Watcher → RAG Incremental Indexing ────────────────────
		if cfg.RAG.WatchEnabled && cfg.RAG.WatchPath != "" {
			watchPath := cfg.RAG.WatchPath
			if !filepath.IsAbs(watchPath) {
				if abs, err := filepath.Abs(watchPath); err == nil {
					watchPath = abs
				}
			}

			repomapGen = repomap.NewGenerator(logger)
			gen := repomapGen
			watcher := repomap.NewWatcher(watchPath, gen, logger)

			var (
				batchMu    sync.Mutex
				batchFiles = make(map[string]struct{})
				batchTimer *time.Timer
			)

			watcher.SetOnChange(func(filePath string) {
				batchMu.Lock()
				batchFiles[filePath] = struct{}{}
				if batchTimer != nil {
					batchTimer.Stop()
				}
				batchTimer = time.AfterFunc(2*time.Second, func() {
					batchMu.Lock()
					files := make([]string, 0, len(batchFiles))
					for f := range batchFiles {
						files = append(files, f)
					}
					batchFiles = make(map[string]struct{})
					batchMu.Unlock()

					for _, f := range files {
						fullPath := filepath.Join(watchPath, f)
						fCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						if _, statErr := os.Stat(fullPath); os.IsNotExist(statErr) {
							if err := idx.DeleteFile(fCtx, watchPath, f); err != nil {
								logger.Warn("incremental delete failed",
									zap.String("file", f), zap.Error(err))
							}
						} else {
							if err := idx.IndexFile(fCtx, watchPath, f); err != nil {
								logger.Warn("incremental index failed",
									zap.String("file", f), zap.Error(err))
							}
						}
						cancel()
					}
					if len(files) > 0 {
						logger.Info("incremental index batch completed",
							zap.Int("files", len(files)))
					}
				})
				batchMu.Unlock()
			})

			go watcher.Start(context.Background())
			logger.Info("file watcher started for RAG incremental indexing",
				zap.String("watch_path", watchPath))
		}
	}

	// ─── Skill Registry ──────────────────────────────────────────────────
	skillReg := skill.NewRegistry(logger)
	orch.SetSkillRegistry(skillReg)
	apiServer.SetSkillRegistry(skillReg)
	logger.Info("skill registry initialized and wired into orchestrator + API")

	// ─── Wire Store into API Server (for dynamic tool persistence) ───────
	if pgStore != nil {
		apiServer.SetStore(pgStore)
		logger.Info("PostgreSQL store wired into API server for dynamic tool persistence")

		// Load persisted dynamic tools on startup
		if records, err := pgStore.LoadDynamicTools(context.Background()); err == nil {
			for _, rec := range records {
				dtCfg := tools.DynamicToolConfig{
					Name:           rec.Name,
					Description:    rec.Description,
					Parameters:     rec.Parameters,
					ExecutorType:   tools.ExecutorType(rec.ExecutorType),
					ExecutorConfig: rec.ExecutorConfig,
					RiskLevel:      rec.RiskLevel,
					CreatedAt:      rec.CreatedAt,
				}
				if tool, err := tools.NewDynamicTool(dtCfg); err == nil {
					if err := orch.RegisterDynamicTool(tool); err == nil {
						logger.Info("loaded dynamic tool from DB", zap.String("name", rec.Name))
					}
				}
			}
		} else {
			logger.Warn("failed to load dynamic tools from DB", zap.Error(err))
		}
	}

	// ─── MCP Gateway → API Server ────────────────────────────────────────
	if mcpGateway != nil {
		apiServer.SetMCPGateway(mcpGateway)
		logger.Info("MCP gateway wired into API server for dynamic management")
	}

	// ─── Wire Workspace Manager + File Tools + Generator ─────────────────
	{
		wsMgr, err := workspace.NewManager("/tmp/agent-workspaces", logger)
		if err != nil {
			logger.Warn("workspace manager init failed, file tools and generator disabled", zap.Error(err))
		} else {
			// Wire autonomous file tools into orchestrator (read_file, write_file, etc.)
			orch.SetWorkspaceManager(wsMgr)
			apiServer.SetWorkspaceManager(wsMgr)
			logger.Info("autonomous file tools enabled in orchestrator (read_file, write_file, patch_file, list_files, create_directory, run_tests)")
			logger.Info("workspace manager wired into API server for file browser/editor")

			// Wire project generator into API server
			gen := generator.NewGenerator(llmClient, sandboxMgr, wsMgr, logger)
			apiServer.SetGenerator(gen)
			logger.Info("project generator wired into API server")
		}
	}

	// ─── [P1] Tree-sitter AST Parser (optional) ─────────────────────────
	if cfg.TreeSitter.Enabled {
		tsParser := treesitter.NewCGOParser(logger)
		orch.SetTreeSitterParser(tsParser)
		if ragEngine != nil {
			ragEngine.SetTreeSitterParser(&tsParserAdapter{parser: tsParser})
		}
		if repomapGen != nil {
			repomapGen.SetTreeSitterParser(&tsRepomapAdapter{parser: tsParser})
		}
		logger.Info("tree-sitter parser initialized", zap.Strings("languages", tsParser.SupportedLanguages()))
	}

	// ─── [P1] PTY Session Manager (optional) ────────────────────────────
	if cfg.PTY.Enabled {
		ptyCfg := pty.ManagerConfig{
			Backend:     cfg.PTY.Backend,
			MaxSessions: cfg.PTY.MaxSessionsPerWorkspace,
			OutputLimit: cfg.PTY.OutputLimit,
			Shell:       cfg.PTY.Shell,
		}
		if ptyCfg.Backend == "" {
			ptyCfg.Backend = "local"
		}
		if ptyCfg.MaxSessions == 0 {
			ptyCfg.MaxSessions = 3
		}
		if ptyCfg.Shell == "" {
			ptyCfg.Shell = "/bin/bash"
		}
		if cfg.PTY.Backend == "docker" && sandboxMgr != nil {
			ptyCfg.Image = cfg.PTY.Image
			ptyCfg.MemoryLimit = cfg.PTY.MemoryLimit
			ptyCfg.CPUQuota = cfg.PTY.CPUQuota
		}
		ptyCfg.WorkspaceBase = "/tmp/agent-workspaces"
		ptyMgr, err := pty.NewManager(ptyCfg, logger)
		if err != nil {
			logger.Warn("PTY manager init failed, shell_exec disabled", zap.Error(err))
		} else {
			orch.SetPTYManager(ptyMgr)
			defer ptyMgr.Close()
			logger.Info("PTY session manager initialized",
				zap.String("backend", ptyCfg.Backend),
				zap.Int("max_sessions", ptyCfg.MaxSessions))
		}
	}

	// ─── [P1] LSP Client (optional) ─────────────────────────────────────
	if cfg.LSP.Enabled {
		lspCfg := lsp.Config{
			Servers: make(map[string]lsp.ServerConfig),
			Timeout: cfg.LSP.MaxConcurrentRequests,
		}
		for name, srv := range cfg.LSP.Servers {
			lspCfg.Servers[name] = lsp.ServerConfig{
				Command:   srv.Command,
				Args:      srv.Args,
				Languages: srv.Languages,
			}
		}
		lspClient := lsp.NewClient(lspCfg, logger)
		orch.SetLSPClient(lspClient)
		defer lspClient.ShutdownAll()
		logger.Info("LSP client initialized", zap.Int("servers", len(cfg.LSP.Servers)))
	}

	httpServer := &http.Server{
		Addr:         cfg.Server.HTTPAddr,
		Handler:      apiServer.Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// ─── [OPT-11] Initialize Temporal Worker (optional) ──────────────────
	// When cfg.Temporal.Host is set, we actually start a Temporal worker that
	// serves the task queue with the workflows + activities defined in
	// internal/temporal/*. If the Temporal server is unreachable we log the
	// failure and continue — the Temporal-gated HITL path is optional and
	// the HTTP/REST path keeps working without it.
	var temporalCli temporalclient.Client
	var temporalWorker temporalworker.Worker
	if cfg.Temporal.Host != "" {
		temporalCli, temporalWorker = startTemporalWorker(&cfg.Temporal, &cfg.Security, orch, logger)
		if temporalCli != nil {
			defer temporalCli.Close()
			queue := cfg.Temporal.TaskQueue
			if queue == "" {
				queue = "agent-tasks"
			}
			orch.SetTemporalClient(&temporalHITLAdapter{client: temporalCli, queue: queue})
			logger.Info("temporal HITL bridge wired into orchestrator")
		}
		if temporalWorker != nil {
			defer temporalWorker.Stop()
		}
	}

	// ─── Start Server ────────────────────────────────────────────────────
	go func() {
		logger.Info("HTTP server starting",
			zap.String("addr", cfg.Server.HTTPAddr),
		)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	// ─── Graceful Shutdown ───────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	logger.Info("shutdown signal received", zap.String("signal", sig.String()))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown error", zap.Error(err))
	}

	// After http.Server.Shutdown returns, no new HTTP handlers will start,
	// but detached ReAct goroutines spawned by handleChat (which use a
	// background-derived agentCtx so client disconnect doesn't kill them)
	// may still be mid-tool-call. Drain blocks until they finish — bounded
	// by shutdownCtx so a stuck agent can't pin the process forever.
	logger.Info("draining in-flight chat goroutines")
	if err := apiServer.Drain(shutdownCtx); err != nil {
		logger.Warn("drain timed out, exiting with detached agents still running", zap.Error(err))
	}

	// Stop background memory decay loop (if started). Doing this AFTER Drain
	// avoids cancelling a Decay() call mid-flight; if one is in progress it
	// will complete its current iteration before the goroutine exits.
	if memoryDistillStop != nil {
		memoryDistillStop()
	}
	if memoryEpisodicGCStop != nil {
		memoryEpisodicGCStop()
	}
	if memoryDecayStop != nil {
		memoryDecayStop()
	}
	// Stop access batcher LAST: its final flush captures the last 5s of
	// access signal from any in-flight chat goroutines drained above.
	// Cancellation triggers a final TouchBatch using a detached 5s
	// context, so the goroutine exits within seconds even if PG is slow.
	if memoryAccessStop != nil {
		memoryAccessStop()
	}
	if memoryPromoteStop != nil {
		memoryPromoteStop()
	}

	// Close Redis
	if err := rdb.Close(); err != nil {
		logger.Error("redis close error", zap.Error(err))
	}

	logger.Info("Code Intelligence Agent stopped gracefully")
}

// startTemporalWorker dials the Temporal server, registers the agent's
// workflow + activities, and starts the worker in the background. Returns
// (client, worker) so main can defer Close/Stop. On any error we log and
// return (nil, nil) — Temporal is optional and the HTTP path must stay up.
func startTemporalWorker(
	cfg *config.TemporalConfig,
	secCfg *config.SecurityConfig,
	orch *orchestrator.Orchestrator,
	logger *zap.Logger,
) (temporalclient.Client, temporalworker.Worker) {
	logger.Info("initializing Temporal worker",
		zap.String("host", cfg.Host),
		zap.String("namespace", cfg.Namespace),
		zap.String("task_queue", cfg.TaskQueue),
	)

	ns := cfg.Namespace
	if ns == "" {
		ns = temporalclient.DefaultNamespace
	}
	queue := cfg.TaskQueue
	if queue == "" {
		queue = "agent-tasks"
	}

	cli, err := temporalclient.Dial(temporalclient.Options{
		HostPort:  cfg.Host,
		Namespace: ns,
	})
	if err != nil {
		logger.Warn("temporal dial failed — HITL workflow path disabled",
			zap.String("host", cfg.Host), zap.Error(err))
		return nil, nil
	}

	w := temporalworker.New(cli, queue, temporalworker.Options{})
	w.RegisterWorkflow(temporalpkg.AgentTaskWorkflow)

	activities := temporalpkg.NewActivities(orch, secCfg, logger)
	// Register all three activity methods on the struct. The activity symbol
	// references (e.g. temporalpkg.ParseIntentActivity) are method-value
	// references; Temporal's activity registry wants the concrete bound
	// methods, which is what RegisterActivity on the receiver yields.
	w.RegisterActivity(activities)

	// worker.Start is non-blocking; it spawns its own polling goroutines.
	// On Stop() the worker drains in-flight activities up to its shutdown
	// timeout (default 1m) before returning.
	if err := w.Start(); err != nil {
		logger.Warn("temporal worker failed to start", zap.Error(err))
		cli.Close()
		return nil, nil
	}
	logger.Info("temporal worker started", zap.String("task_queue", queue))
	return cli, w
}
