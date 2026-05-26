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
	"syscall"
	"time"

	"github.com/agent/code_agent/internal/api"
	"github.com/agent/code_agent/internal/auth"
	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/generator"
	"github.com/agent/code_agent/internal/indexer"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/mcp"
	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/multiagent"
	"github.com/agent/code_agent/internal/orchestrator"
	"github.com/agent/code_agent/internal/planner"
	"github.com/agent/code_agent/internal/rag"
	"github.com/agent/code_agent/internal/sandbox"
	"github.com/agent/code_agent/internal/session"
	"github.com/agent/code_agent/internal/skill"
	"github.com/agent/code_agent/internal/store"
	temporalpkg "github.com/agent/code_agent/internal/temporal"
	"github.com/agent/code_agent/internal/tools"
	"github.com/agent/code_agent/internal/tracing"
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
	llmClient, err := llm.NewClient(&cfg.LLM, logger)
	if err != nil {
		logger.Fatal("failed to initialize LLM client", zap.Error(err))
	}
	logger.Info("LLM client initialized",
		zap.String("primary", cfg.LLM.Primary.Provider+"/"+cfg.LLM.Primary.Model),
	)

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
		embedder = rag.NewOpenAIEmbedder(&cfg.RAG, &cfg.LLM.Primary, logger)
	case "local":
		embedder = rag.NewLocalHashEmbedder(cfg.Qdrant.VectorSize, logger)
	default:
		logger.Warn("unknown embedding_provider, using local", zap.String("provider", embeddingProvider))
		embedder = rag.NewLocalHashEmbedder(cfg.Qdrant.VectorSize, logger)
	}

	qdrantStore, err := rag.NewQdrantStore(&cfg.Qdrant, logger)
	if err != nil {
		logger.Warn("Qdrant not available, RAG disabled", zap.Error(err))
	} else {
		// Initialize reranker if configured
		var reranker rag.Reranker
		if cfg.RAG.RerankEnabled && cfg.RAG.RerankModel != "" && cfg.RAG.RerankBaseURL != "" {
			reranker = rag.NewAPIReranker(&cfg.RAG, logger)
		}

		ragEngine = rag.NewEngine(embedder, qdrantStore, reranker, &cfg.RAG, logger)
		defer qdrantStore.Close()
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
	mcpGateway, err := mcp.NewGateway(&cfg.MCP, logger)
	if err != nil {
		logger.Warn("MCP gateway initialization failed", zap.Error(err))
	} else {
		defer mcpGateway.Close()
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
		jwtCfg := &auth.JWTConfig{
			SecretKey:     cfg.Auth.JWTSecret,
			Issuer:        cfg.Auth.JWTIssuer,
			TokenExpiry:   24 * time.Hour,
			RefreshExpiry: 7 * 24 * time.Hour,
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
	// Wire Planner for complex task DAG execution
	plannerAdapter := orchestrator.NewLLMCallerAdapter(llmClient)
	p := planner.NewPlanner(plannerAdapter, logger)
	orch.AttachPlanner(p)
	logger.Info("orchestrator initialized (planner attached)")

	// Wire multi-agent Supervisor for parallel plan execution
	supCfg := multiagent.DefaultSupervisorConfig()
	supervisor := multiagent.NewSupervisor(orch.NewToolDispatcherAdapter(), supCfg, logger)
	orch.AttachSupervisor(supervisor)
	logger.Info("multi-agent supervisor attached")

	// ─── Wire Memory Store (optional) ───────────────────────────────────
	memAdapter := NewMemoryAdapter(rdb, pgStore, embedder, logger)
	if memAdapter != nil {
		orch.SetMemoryStore(memAdapter)
		logger.Info("long-term memory store wired into orchestrator")
	}

	// ─── Wire Memory Extractor (optional) ────────────────────────────────
	if memAdapter != nil {
		// Get the underlying HybridStore from adapter to create extractor
		// The extractor needs the store interface and LLM client
		memoryExtractor := memory.NewExtractor(memAdapter.HybridStore(), llmClient, logger)
		orch.SetMemoryExtractor(memoryExtractor)
		logger.Info("memory extractor wired into orchestrator")
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

	// ─── Wire Indexer into API Server ────────────────────────────────────
	if ragEngine != nil {
		idx := indexer.NewIndexer(ragEngine, &cfg.RAG, logger)
		apiServer.SetIndexer(idx)
		logger.Info("indexer wired into API server")
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
