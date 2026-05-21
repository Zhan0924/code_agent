# 🧠 Code Intelligence Agent (生产级代码智能 Agent)

A production-grade, Go-based intelligent agent system featuring deep code RAG, containerized sandboxes, MCP tool expansion, and Temporal-driven stateful workflows with Human-in-the-Loop (HITL) support.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│                    API Gateway (Gin)                     │
│     REST / WebSocket / SSE / Prometheus /metrics         │
│  ┌──────────────────────────────────────────────────┐   │
│  │  Middleware: RequestID · RateLimit · Metrics · HMAC│   │
│  └──────────────────────────────────────────────────┘   │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  ┌─────────────┐  ┌──────────┐  ┌───────────────────┐  │
│  │ Orchestrator │──│ LLM      │  │ Temporal Workflow  │  │
│  │ (FSM Brain)  │  │ Client   │  │ (HITL + Stateful) │  │
│  └──────┬───────┘  └──────────┘  └───────────────────┘  │
│         │                                               │
│  ┌──────┼──────────────────────────────────────────┐    │
│  │      ▼                                          │    │
│  │  ┌────────┐  ┌──────────┐  ┌──────────────┐    │    │
│  │  │  RAG   │  │ Sandbox  │  │ MCP Gateway  │    │    │
│  │  │ Engine │  │ (Docker) │  │ (JSON-RPC)   │    │    │
│  │  └───┬────┘  └────┬─────┘  └──────┬───────┘    │    │
│  │      │            │               │             │    │
│  │  ┌───▼────┐  ┌────▼─────┐  ┌──────▼───────┐    │    │
│  │  │ Qdrant │  │ Docker   │  │ GitHub/Jira  │    │    │
│  │  │ Vector │  │ Engine   │  │ MCP Servers  │    │    │
│  │  └────────┘  └──────────┘  └──────────────┘    │    │
│  └─────────────────────────────────────────────────┘    │
│                                                         │
│  ┌──────────┐  ┌────────────┐  ┌──────────────┐        │
│  │  Redis   │  │ PostgreSQL │  │   Temporal   │        │
│  │ (Session)│  │ (Metadata) │  │   (Workflow) │        │
│  └──────────┘  └────────────┘  └──────────────┘        │
└─────────────────────────────────────────────────────────┘
```

## Core Subsystems

| Subsystem | Description | Package |
|-----------|-------------|---------|
| **Orchestrator** | FSM-based task planner with intent classification and HITL | `internal/orchestrator` |
| **LLM Client** | Circuit-breaker protected, multi-provider with fallback | `internal/llm` |
| **RAG Engine** | AST-aware code parsing, dual-recall (dense+sparse), reranking | `internal/rag` |
| **Sandbox** | Docker-based ephemeral execution with cgroups limits & I/O streaming | `internal/sandbox` |
| **MCP Gateway** | JSON-RPC 2.0 client for Model Context Protocol tool expansion | `internal/mcp` |
| **Session Manager** | Redis-backed sliding window context with hot/cold separation & key sharding | `internal/session` |
| **Temporal Workflows** | Stateful, durable workflows with HITL signal-based approval | `internal/temporal` |
| **API Server** | Gin HTTP server with REST, WebSocket, SSE, rate limiting, and Prometheus metrics | `internal/api` |
| **Token Pruner** | Multi-signal AST-aware context compression engine | `internal/context` |
| **Prompt Builder** | KV Cache-friendly prompt assembly with immutable prefix optimization | `internal/context` |
| **Structured Errors** | Typed errors with HTTP status mapping for consistent API responses | `internal/errors` |
| **HMAC Security** | HMAC-SHA256 webhook signature verification with replay attack prevention | `internal/security` |
| **Egress Control** | Network egress policy engine (CIDR whitelist, iptables rule generation) | `internal/security` |
| **Object Pools** | sync.Pool-based GC optimization for byte slices, buffers, JSON encoders | `internal/pool` |
| **Metrics** | Prometheus metric definitions for LLM, RAG, sandbox, MCP, session, API | `internal/metrics` |
| **Config Validation** | Multi-field configuration validation with detailed error reporting | `internal/config` |

## Quick Start

### Prerequisites

- Go 1.23+
- Docker (for sandbox execution)
- Redis (for session management)

### Local Development

```bash
# Clone and enter directory
cd code_agent

# Install dependencies
go mod tidy

# Start infrastructure
docker compose up -d redis postgres qdrant temporal

# Run the agent
go run ./cmd/agent --config configs/config.yaml
```

### Docker Compose (Full Stack)

```bash
docker compose up -d
```

### Build & Test

```bash
make build          # Build binary
make test           # Run tests with race detector
make lint           # Run linter
make docker-build   # Build Docker image
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/chat` | Synchronous chat completion |
| `POST` | `/api/v1/chat/stream` | SSE streaming chat |
| `GET`  | `/api/v1/ws` | WebSocket bidirectional chat |
| `POST` | `/api/v1/sessions` | Create new session |
| `GET`  | `/api/v1/sessions/:id` | Get session details |
| `DELETE` | `/api/v1/sessions/:id` | Delete session |
| `POST` | `/api/v1/tasks/:id/approve` | Approve/reject HITL task |
| `POST` | `/api/v1/webhooks/mcp-callback` | HMAC-protected MCP callback |
| `POST` | `/api/v1/webhooks/ci-callback` | HMAC-protected CI/CD callback |
| `GET`  | `/healthz` | Liveness probe |
| `GET`  | `/readyz` | Readiness probe (checks Redis connectivity) |
| `GET`  | `/metrics` | Prometheus metrics endpoint |

### Example: Chat Request

```bash
curl -X POST http://localhost:8080/api/v1/chat \
  -H "Content-Type: application/json" \
  -d '{"message": "Explain the authentication flow in auth-service"}'
```

### Example: WebSocket

```javascript
const ws = new WebSocket('ws://localhost:8080/api/v1/ws');
ws.onmessage = (event) => console.log(JSON.parse(event.data));
ws.send(JSON.stringify({ type: 'chat', message: 'Run: print("hello")' }));
```

## Observability

### Prometheus Metrics

The `/metrics` endpoint exposes comprehensive metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `code_agent_llm_request_total` | Counter | LLM API requests by provider/status |
| `code_agent_llm_request_duration_seconds` | Histogram | LLM API latency P50/P90/P99 |
| `code_agent_llm_tokens_used_total` | Counter | Token consumption (prompt/completion) |
| `code_agent_llm_circuit_breaker_state` | Gauge | Circuit breaker state (0=closed, 2=open) |
| `code_agent_rag_retrieval_duration_seconds` | Histogram | RAG retrieval latency |
| `code_agent_sandbox_execution_total` | Counter | Sandbox runs by language/outcome |
| `code_agent_session_active_count` | Gauge | Active session count |
| `code_agent_hitl_pending_count` | Gauge | Tasks awaiting human approval |
| `code_agent_api_request_total` | Counter | API requests by method/path/status |
| `code_agent_api_request_duration_seconds` | Histogram | API latency per endpoint |

### Request Tracing

Every request is tagged with `X-Request-ID` (auto-generated UUID or forwarded from upstream).

## Security

- **Rate Limiting**: Token-bucket per-IP rate limiter (configurable RPS + burst)
- **HMAC Webhook Verification**: SHA-256 signature + timestamp-based replay attack prevention
- **Egress Control**: CIDR-based network whitelist with cloud metadata endpoint blocking
- **Sensitive Pattern Detection**: Regex-based interception of dangerous operations (`DROP DATABASE`, `kubectl delete`, etc.)
- **HITL Approval**: Critical operations suspended until human approval via Temporal signal
- **Container Isolation**: Code executes in ephemeral containers with `NetworkMode: none`, memory/CPU limits
- **Circuit Breaker**: LLM API failures trigger automatic fallback to local model

## Configuration

Configuration is loaded from `configs/config.yaml` with environment variable overrides using the `CODE_AGENT_` prefix:

```bash
export CODE_AGENT_LLM_PRIMARY_API_KEY=sk-xxx
export CODE_AGENT_REDIS_ADDR=redis:6379
```

Configuration is validated at startup — missing required fields or invalid ranges cause immediate failure with detailed error messages.

## Deployment

### Kubernetes

```bash
kubectl apply -f deployments/k8s/deployment.yaml
```

Features:
- HPA auto-scaling (CPU/memory-based)
- Liveness & readiness probes
- ConfigMap for configuration
- Secrets for API keys
- Stateless compute nodes

## Project Structure

```
code_agent/
├── cmd/agent/main.go              # Entry point with dependency wiring
├── configs/config.yaml            # Configuration
├── internal/
│   ├── api/                       # HTTP handlers, middleware, WebSocket, SSE
│   │   ├── router.go             # Route registration + Prometheus endpoint
│   │   ├── handlers.go           # Request handlers + webhook handlers
│   │   └── middleware.go         # RequestID, RateLimit, Metrics, Recovery
│   ├── config/                    # Configuration loading + validation
│   │   ├── config.go             # Viper-based config with env override
│   │   └── validate.go           # Multi-field validation
│   ├── context/                   # Token optimization
│   │   ├── pruner.go             # AST-aware token pruning engine
│   │   └── prompt_builder.go     # KV Cache-friendly prompt assembly
│   ├── errors/                    # Structured error types
│   │   └── errors.go             # Typed errors with HTTP status mapping
│   ├── llm/                       # LLM client with circuit breaker
│   │   ├── client.go             # Multi-provider with fallback
│   │   └── openai_provider.go    # OpenAI-compatible provider
│   ├── mcp/                       # MCP JSON-RPC 2.0 gateway
│   │   └── client.go             # Server lifecycle + tool registry
│   ├── metrics/                   # Observability
│   │   └── metrics.go            # Prometheus metric definitions
│   ├── models/                    # Domain models
│   │   └── models.go             # Shared types
│   ├── orchestrator/              # Core brain (FSM + HITL)
│   │   └── orchestrator.go       # Intent → Plan → Execute loop
│   ├── pool/                      # GC optimization
│   │   └── pool.go               # sync.Pool for byte slices, buffers, JSON
│   ├── rag/                       # Code RAG engine
│   │   ├── engine.go             # Dual-recall + rerank orchestration
│   │   ├── ast_parser.go         # AST-aware code chunking
│   │   └── qdrant_store.go       # Qdrant vector store client
│   ├── sandbox/                   # Docker sandbox manager
│   │   └── manager.go            # Ephemeral container lifecycle
│   ├── security/                  # Security subsystem
│   │   ├── hmac.go               # HMAC-SHA256 signature verification
│   │   └── egress.go             # Network egress policy engine
│   ├── session/                   # Redis session management
│   │   └── manager.go            # Hot/cold separation + key sharding
│   └── temporal/                  # Temporal workflow definitions
│       ├── workflows.go          # Agent task workflow with HITL
│       └── activities.go         # Temporal activity implementations
├── deployments/k8s/               # Kubernetes manifests
├── Dockerfile                     # Multi-stage production build
├── docker-compose.yml             # Full local stack
├── Makefile                       # Build automation
└── go.mod                         # Go module definition
```

## Tech Stack

| Layer | Technology | Purpose |
|-------|-----------|---------|
| Language | **Go 1.23** | High concurrency, low memory |
| API Framework | **Gin** | High-performance HTTP routing |
| Workflow Engine | **Temporal** | Stateful HITL workflows |
| Vector DB | **Qdrant** | Millisecond semantic search |
| Cache/Session | **Redis** | Distributed session & locks |
| Database | **PostgreSQL** | Configuration & metadata |
| Sandbox | **Docker Engine API** | Isolated code execution |
| LLM | **OpenAI-compatible** | GPT-4o + local fallback |
| Protocol | **MCP (JSON-RPC 2.0)** | Extensible tool ecosystem |
| Metrics | **Prometheus** | Full observability |
| Resilience | **gobreaker** | Circuit breaker pattern |

## License

MIT
