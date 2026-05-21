# 20 · 部署、CI 与工程化 `Dockerfile` / `docker-compose` / `k8s` / `CI` / `Makefile`

> 代码：
> - `Dockerfile` (~40 行) — 生产多阶段构建
> - `Dockerfile.allinone` (~95 行) — 单镜像全栈（PG + Redis + Qdrant + Temporal + Jaeger + DinD）
> - `Dockerfile.local` / `Dockerfile.test` — 开发 / 测试专用镜像
> - `docker-compose.yml` (~130 行) — 6 服务本地编排
> - `deploy/entrypoint.sh` (~150 行) — allinone 镜像 7 阶段启动脚本
> - `deploy/qdrant.yaml` / `deploy/redis.conf` — 嵌入式服务配置
> - `deploy/download_jaeger.sh` — 预置 Jaeger 二进制
> - `deployments/k8s/deployment.yaml` (~130 行) — K8s 清单合集（Deployment + Service + HPA + SA + Secret）
> - `.github/workflows/ci.yml` (~95 行) — 4-job CI 流水线
> - `Makefile` (~80 行) — 14 个 target
> - `test_*.sh` (4 个集成测试脚本)

---

## 1. 模块定位

**"如何把一个 Go 二进制变成生产就绪的系统"** —— 本章涵盖从**本地开发** → **单机 Demo** → **K8s 生产**的三种部署形态，以及贯穿其中的 CI/CD、构建工程化。

**三种目标形态**：

| 形态 | 用例 | 镜像 | 启动方式 |
|---|---|---|---|
| **Local Dev** | 开发者本机 | `Dockerfile.local` | `make run` |
| **Compose Demo** | 演示 / 单机部署 / 小团队共享 | `Dockerfile` + `docker-compose.yml` | `docker compose up` |
| **AllInOne** | 一键体验、销售 Demo、无网络环境 | `Dockerfile.allinone` | `docker run --privileged -p 8080:8080 ...` |
| **K8s Prod** | 生产环境、多副本、HPA | `Dockerfile` + `deployments/k8s/*.yaml` | `kubectl apply -f` |

---

## 1.5 核心设计问题

### 为什么三种部署形态？

| 形态 | 用途 | 不适合 |
|---|---|---|
| Local Dev | 开发者本机迭代 | 演示给外人 |
| Compose Demo | 单机演示 / 小团队 / QA | 多用户生产 |
| AllInOne | 销售演示 / 离线环境 | 真正生产 |
| K8s Prod | 多副本生产 / HPA | 开发机器 |

三种形态共用同一 binary，**仅配置不同**——不是"为每个环境写一套代码"。
好处：生产里出 bug 本地能复现。

---

## 2.5 数据流总览

```text
═══════════════ 构建流水线 (CI → 镜像) ═══════════════

┌──────────────┐     ┌───────────────────────────────────────┐
│  git push    │──▶  │ CI Pipeline (4-job DAG)               │
└──────────────┘     └───────────────────┬───────────────────┘
                                         │
                     ┌───────────────────┬┴──────────────────┐
                     ▼                   ▼                   │
              ┌────────────┐     ┌────────────┐             │
              │  lint      │     │  test      │             │
              │ golangci-  │     │ go test    │             │
              │ lint       │     │ +redis/pg  │             │
              │            │     │ sidecars   │             │
              └─────┬──────┘     └─────┬──────┘             │
                    │                  │                    │
                    └────────┬─────────┘                    │
                             ▼ (both pass)                  │
              ┌────────────────────────────────┐            │
              │  build + docker                │            │
              │  多阶段 Dockerfile:            │            │
              │   Stage1 (builder):            │            │
              │     go mod download (缓存层)   │            │
              │     CGO_ENABLED=0 go build     │            │
              │   Stage2 (runtime):            │            │
              │     alpine:3.20 (minimal ~30MB)│            │
              │     COPY binary + USER agent   │            │
              └─────────────────┬──────────────┘            │
                                │                           │
                                ▼                           │
                     ┌─────────────────────┐               │
                     │  Registry Push      │               │
                     │  (main branch only) │               │
                     └─────────────────────┘               │


═══════════════ K8s 生产部署 ═══════════════

┌─────────────────────────────────────────────────────────────┐
│ Kubernetes Cluster                                           │
│                                                              │
│  ┌───────────────────────────────────────────────────────┐  │
│  │ Deployment: code-agent (replicas: 2~10 via HPA)       │  │
│  │  ┌─────────────────────────────────────────────────┐  │  │
│  │  │ Pod                                             │  │  │
│  │  │  /healthz → liveness (重启无响应 Pod)           │  │  │
│  │  │  /readyz  → readiness (摘除 Redis 断连 Pod)    │  │  │
│  │  │  env: CODE_AGENT_* (from Secrets)              │  │  │
│  │  └─────────────────────────────────────────────────┘  │  │
│  └───────────────────────────────────────────────────────┘  │
│                          │                                   │
│                          ▼                                   │
│  ┌─────────────┐  ┌──────────┐  ┌────────┐  ┌──────────┐  │
│  │ Redis       │  │PostgreSQL│  │Qdrant  │  │Temporal  │  │
│  │ (必需)      │  │(可选)    │  │(可选)  │  │(可选)    │  │
│  └─────────────┘  └──────────┘  └────────┘  └──────────┘  │
└─────────────────────────────────────────────────────────────┘


═══════════════ AllInOne 启动序列 ═══════════════

┌─────────────────────────────────────────────────────────────┐
│ entrypoint.sh (7 阶段, 各带健康探测)                         │
│                                                              │
│  ① PostgreSQL  ──(pg_isready)──▶ ready                      │
│  ② Redis       ──(redis-cli ping)──▶ ready                  │
│  ③ Qdrant      ──(curl /health)──▶ ready (软降级)           │
│  ④ Temporal    ──(tctl cluster health)──▶ ready (软降级)    │
│  ⑤ Jaeger      ──(curl :16686)──▶ ready (软降级)           │
│  ⑥ Docker-in-Docker ──(docker info)──▶ ready (软降级)       │
│  ⑦ code-agent  ──(/healthz)──▶ ready ★                     │
│                                                              │
│  任何 ③-⑥ 失败 → 跳过(降级可用)                              │
│  ①② 失败 → exit 1 (核心依赖不可缺)                          │
└─────────────────────────────────────────────────────────────┘
```

### 多阶段 Dockerfile 的三个关键点

```dockerfile
# Stage 1: builder
FROM golang:1.25-alpine AS builder
RUN go mod download      ← 单独一层缓存 deps（改业务代码不重下）
COPY . .
RUN go build -ldflags="-w -s" ...

# Stage 2: runtime  — 从 builder 只拷贝二进制
FROM alpine:3.20
COPY --from=builder /build/bin/code-agent /usr/local/bin/
USER agent               ← 非 root 运行
```

关键点：
1. **deps 缓存**：go.mod / go.sum 先复制、单独一层下载，源码改动不会
   重新下 deps（构建时间从 3min 降到 10s）。
2. **镜像瘦身**：runtime 不含 Go 工具链，final 镜像 ~30MB 而非 ~1GB。
3. **非 root**：`USER agent`——即使容器被入侵，攻击者不是 root。

### Kubernetes 上线清单

最少必备（见 `deployments/k8s/deployment.yaml`）：
- `livenessProbe: /healthz`（重启无响应 Pod）
- `readinessProbe: /readyz`（踢出 Redis 断连的 Pod）
- `resources.requests / limits`（HPA 基础 + 防被 OOM killed 相邻 Pod）
- HPA（基于 CPU + 自定义指标如 LLM 队列长度）
- `ServiceAccount` + `Secrets`（敏感配置不放 ConfigMap）
- `PodDisruptionBudget`（升级时保证至少 N 个副本）

### 为什么要 allinone 镜像？

**销售 demo 场景**：客户环境没 Docker Compose、没 K8s、没外网。需要
"`docker run -p 8080:8080 code-agent:allinone`" 一条命令起整套栈。

代价：镜像体积大（~500MB，含 PG + Redis + Qdrant + Temporal + Jaeger），
启动慢（10-30s），不适合真正生产。

---

## 2. 形态一：生产多阶段 Dockerfile

```dockerfile
# Stage 1: builder  ─────────────────────────────────────
FROM dockerhub.icu/library/golang:1.25-alpine AS builder
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download                              # ← 单独层，依赖缓存友好
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s -X main.Version=$(git describe --tags --always)" \
    -o /build/bin/code-agent ./cmd/agent

# Stage 2: runtime  ─────────────────────────────────────
FROM dockerhub.icu/library/alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates tzdata curl && \
    addgroup -S agent && adduser -S agent -G agent
COPY --from=builder /build/bin/code-agent /usr/local/bin/code-agent
COPY --from=builder /build/configs /etc/code-agent/configs
USER agent                                        # ← 非 root
WORKDIR /home/agent
EXPOSE 8080 8081
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1
ENTRYPOINT ["code-agent"]
CMD ["--config", "/etc/code-agent/configs/config.yaml"]
```

### 2.1 8 个关键决策

| 点 | 选择 | 理由 |
|---|---|---|
| **多阶段** | builder (800MB+) → runtime (<30MB) | 剔除 Go toolchain、源码，只保留二进制 |
| **基镜像** | `alpine:3.20` | ~5MB；比 `scratch` 好（有 `ca-certificates`、`tzdata`、`/bin/sh` 可调试） |
| **镜像源** | `dockerhub.icu/library/...` | 国内 GFW 下访问可靠；CI 时切回官方 |
| **缓存分层** | 先 `go.mod`，后 `COPY .` | 改业务代码不用重下依赖；节省 CI 时间 30%+ |
| **静态链接** | `CGO_ENABLED=0` | 无 glibc 依赖；二进制可跨发行版跑 |
| **剥符号** | `-ldflags="-w -s"` | 体积减 30%；prod 不需要 debug 信息 |
| **版本注入** | `-X main.Version=$(git describe)` | `/healthz` 能返回精确构建版本 |
| **非 root** | `USER agent` | 容器逃逸缓解；满足 K8s `PodSecurityPolicy` |
| **HEALTHCHECK** | curl `/healthz` | Docker 感知；K8s 额外有 liveness/readiness |

---

## 3. 形态二：AllInOne 镜像 —— 一键体验

### 3.1 构建方式

```bash
# 1) 先在宿主机交叉编译（Alpine 里没 Go 版本限制困扰）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-w -s" \
    -o bin/code-agent-linux-arm64 ./cmd/agent

# 2) 下载 Jaeger / Qdrant / Temporal 的预编译二进制到 deploy/bin/
bash deploy/download_jaeger.sh

# 3) 构建 allinone 镜像
docker build -f Dockerfile.allinone -t code-agent:allinone .

# 4) 运行（sandbox 需要 --privileged 跑 DinD）
docker run -d --privileged -p 8080:8080 code-agent:allinone
```

### 3.2 Dockerfile.allinone 内装了什么

一镜像封装 **6 个独立服务** + **8 个端口**：

| 服务 | 版本 | 用途 | 端口 |
|---|---|---|---|
| **PostgreSQL** | 16 | 配置 / 审计持久化 | 5432 |
| **Redis** | latest | Session / 限流 / 缓存 | 6379 |
| **Qdrant** | 预编译 binary | 向量检索 | 6333/6334 |
| **Temporal** | `start-dev` 模式 | 工作流引擎 | 7233 |
| **Jaeger** | `all-in-one` badger 后端 | 分布式追踪 | 16686/4317 |
| **Docker (DinD)** | `dockerd` 嵌入 | 代码沙箱 | — (unix socket) |
| **Code Agent** | 本项目 | 主服务 | 8080 |

### 3.3 镜像内**构建时**做了的事

```
FROM alpine:local
    apk add redis postgresql16 docker go python3 nodejs git ...
    mkdir data dirs, chown postgres/redis
    initdb /var/lib/postgresql/data
      → pg_hba.conf: host all all 0.0.0.0/0 md5
      → postgresql.conf: listen_addresses='*', max_conn=50, shared_buffers=64MB
    pg_ctl start
      → CREATE USER agent WITH PASSWORD 'agent_secret' CREATEDB
      → CREATE DATABASE code_agent OWNER agent
    pg_ctl stop                              ← 初始化完关掉，运行时再启
    COPY deploy/bin/qdrant      /usr/local/bin/
    COPY deploy/bin/temporal    /usr/local/bin/
    COPY deploy/bin/jaeger-all-in-one /usr/local/bin/
    COPY bin/code-agent-linux-arm64 /usr/local/bin/code-agent
    COPY configs/config.allinone.yaml ...
    COPY deploy/entrypoint.sh
```

**关键点**：把 "schema 初始化" 移到**构建时**（而不是启动时）—— 首次 `docker run` 也能秒启动。

### 3.4 `entrypoint.sh` 7 阶段启动

```
[1/7] PostgreSQL         → pg_ctl start → CREATE USER/DATABASE if missing
[2/7] Redis              → redis-server --daemonize → PING 探活（15s 内）
[3/7] Qdrant             → 后台启 → curl /healthz 探活（30s 内，失败软降级）
[4/7] Temporal           → start-dev → /health 探活（失败软降级）
[5/7] Jaeger             → OTLP gRPC :4317 + UI :16686 → 探活（失败软降级）
[6/7] Docker daemon      → 三分支：
                            a) 挂载宿主 docker.sock → 直接用
                            b) 无挂载但 --privileged → 启 DinD（fuse-overlayfs/vfs）
                            c) 都没有 → 软降级，禁 sandbox
[7/7] exec code-agent    → 前台运行（Docker 才能 tracing 到主进程生命周期）
```

**设计点**：

| 点 | 实现 |
|---|---|
| **软降级** | 每个 service 启动失败不 `exit 1`，只 warn；例如 Jaeger down → tracing 禁用，其他正常 |
| **幂等初始化** | 检查 `PG_VERSION` / `pg_roles` / `pg_database`；已存在就跳过 |
| **健康探测** | `curl /healthz` + `kill -0 $PID` 双保险；任一存活即算 ready |
| **PID 持有** | 后台 service 保留 PID；`kill -0` 随时可查状态 |
| **exec 收尾** | 最后 `exec code-agent`；主进程 PID=1，信号能直达 |
| **DinD 降级存储驱动** | 优先 `fuse-overlayfs`，退到 `vfs` —— `--privileged` 可能缺内核模块 |

### 3.5 AllInOne 适用 / 不适用场景

| 场景 | 适用 |
|---|---|
| 给客户、销售做 3 分钟 demo | ✅ |
| 新员工一键上手 | ✅ |
| 机器学习/AI hackathon 参赛 demo | ✅ |
| 测试环境 | ⚠️ 数据易丢（volume 未命名） |
| **生产** | ❌ 单点故障；无法水平扩展；PG/Redis 不该和应用同镜像 |

---

## 4. 形态三：docker-compose.yml 多服务编排

### 4.1 服务拓扑

```
┌────────────────────────────────────────────┐
│  agent-net (bridge)                        │
│                                            │
│  ┌─────────┐  ┌───────┐  ┌──────────┐     │
│  │  agent  │→ │ redis │  │ postgres │     │
│  │  :8080  │  └───────┘  └──────────┘     │
│  └────┬────┘                               │
│       ↓                                    │
│  ┌─────────┐  ┌──────────┐  ┌───────────┐ │
│  │ qdrant  │  │ temporal │  │temporal-ui│ │
│  │:6333/34 │  │  :7233   │  │  :8088    │ │
│  └─────────┘  └──────────┘  └───────────┘ │
│                                            │
│  volumes: redis-data / pg-data / qdrant-data│
└────────────────────────────────────────────┘
```

### 4.2 关键细节

- **服务发现**：agent 通过 `CODE_AGENT_REDIS_ADDR=redis:6379` 这种 env 覆盖，借用 Viper 的 `AutomaticEnv()`（见 01_config）。Docker DNS 解析 `redis` → 容器 IP。
- **启动依赖**：`depends_on + condition: service_healthy`；只有 redis/pg 的 healthcheck 过了，agent 才启动。
- **sandbox 支持**：`volumes: /var/run/docker.sock:/var/run/docker.sock` + `user: "0:0"`（必须 root 才能读 socket）。
- **Temporal 数据库隔离**：Temporal 也跑在 postgres 但用独立 DB `temporal`，避免 schema 冲突。
- **镜像源**：`docker.m.daocloud.io/...` 国内镜像加速。
- **数据持久化**：`volumes` 命名卷，`docker compose down -v` 才清除。
- **Temporal UI**：`temporal-ui:8088` 独立容器，方便可视化看 workflow。

### 4.3 Dev 启停

```bash
make docker-up       # docker compose up -d
make docker-down     # docker compose down -v  (注意 -v 清数据!)
docker compose logs -f agent
docker compose exec agent sh
```

---

## 5. 形态四：K8s 生产清单

### 5.1 `deployments/k8s/deployment.yaml` 拆解

一个文件包含 **5 个 K8s 对象**（`---` 分隔）：

| 对象 | 用途 |
|---|---|
| `Deployment` | 主 pod（2 副本起步） |
| `Service` (ClusterIP :8080) | 集群内负载均衡入口 |
| `HorizontalPodAutoscaler` v2 | 基于 CPU/内存自动扩缩 |
| `ServiceAccount code-agent` | pod 身份（未来接 RBAC） |
| `Secret code-agent-secrets` | API keys |

### 5.2 Deployment 精要

```yaml
replicas: 2                                    # ← 双 AZ 最小配置
containers:
- name: code-agent
  image: code-agent:latest
  env:
    - name: CODE_AGENT_LLM_PRIMARY_API_KEY
      valueFrom:
        secretKeyRef:                          # ← 从 Secret 注入
          name: code-agent-secrets
          key: openai-api-key
    - name: CODE_AGENT_REDIS_ADDR
      value: "redis-cluster:6379"              # ← 依赖独立部署的 Redis
    - name: CODE_AGENT_QDRANT_ADDR
      value: "qdrant:6334"
  resources:
    requests: { cpu: 250m, memory: 256Mi }     # ← 调度器据此选节点
    limits:   { cpu: 1000m, memory: 512Mi }    # ← 超限被 throttle/OOMKilled
  livenessProbe:                               # ← 活着吗？死了就重启
    httpGet: { path: /healthz, port: http }
    initialDelaySeconds: 10
    periodSeconds: 30
  readinessProbe:                              # ← 能接流量吗？no=从 LB 摘
    httpGet: { path: /readyz, port: http }
    initialDelaySeconds: 5
    periodSeconds: 10
  volumeMounts:
    - name: config
      mountPath: /etc/code-agent/configs
      readOnly: true                           # ← 配置只读挂载
```

**设计点**：

- **`readinessProbe` 独立于 `livenessProbe`**：启动慢（要连 DB / LLM）时只是 `readyz` 不通，流量不进来；但 `healthz` 通过，不重启；
- **readOnly 配置挂载**：配置由 ConfigMap 提供，容器内改了也丢；遵循"不可变基础设施"；
- **resource requests/limits 不等**：limits 是 requests 的 ~4x，允许突发；requests 小能让调度器紧凑装箱。

### 5.3 HPA v2 多指标

```yaml
minReplicas: 2
maxReplicas: 10
metrics:
  - type: Resource
    resource: { name: cpu, target: { averageUtilization: 70 } }
  - type: Resource
    resource: { name: memory, target: { averageUtilization: 80 } }
```

**任一**指标超阈值就扩；**全部**低于阈值才缩。生产建议加第三维**自定义指标** `hitl_pending_count`，让 HITL 排队时自动扩容。

### 5.4 K8s 生产 **缺失项** (开发者需自补)

| 缺 | 原因 | 建议 |
|---|---|---|
| `PodDisruptionBudget` | 未列 | `maxUnavailable: 1` 保滚动更新时至少 1 pod 活 |
| `NetworkPolicy` | 未列 | 限定 agent → redis / qdrant / pg 的入口 |
| `Ingress` / `IngressRoute` | 未列 | 对外暴露（TLS + domain） |
| `ConfigMap` 定义 | 只引用未定义 | 需独立清单 |
| `PodAntiAffinity` | 未列 | 2 副本应分散节点/AZ |
| `securityContext` | 未列 | `runAsNonRoot: true, readOnlyRootFilesystem: true, seccomp: RuntimeDefault` |
| `topologySpreadConstraints` | 未列 | 更精细的跨 zone 分布 |
| `ServiceMonitor` (Prometheus Operator) | 未列 | Prom 抓 /metrics |

这些建议在后续 `k8s/production/` 目录中补全。

---

## 6. CI 流水线 `.github/workflows/ci.yml`

### 6.1 4 Job DAG

```
┌────────┐     ┌────────┐
│  lint  │     │  test  │
└────┬───┘     └────┬───┘
     │              │
     └──────┬───────┘
            ▼
      ┌──────────┐       ┌────────────┐
      │  build   │       │  docker    │   (only main branch)
      └──────────┘       └────────────┘
```

### 6.2 各 Job 要点

| Job | 干什么 | 关键 |
|---|---|---|
| **lint** | golangci-lint v1.57, `--timeout=5m` | 所有 PR 必过 |
| **test** | `go test ./... -race -coverprofile` | **启动 sidecar**：redis + postgres；通过 env 让集成测试连 |
| **build** | 静态构建 + artifact 上传 | PR 也跑，确保能编译 |
| **docker** | `docker build` | **只在 main** 分支；未 push（留给 release 流程） |

### 6.3 test job 的 sidecar 模式

```yaml
services:
  redis:
    image: redis:7-alpine
    ports: ["6379:6379"]
    options: >-
      --health-cmd "redis-cli ping"
      --health-interval 10s
  postgres:
    image: postgres:16-alpine
    ports: ["5432:5432"]
    env:
      POSTGRES_USER: agent
      POSTGRES_PASSWORD: agent_test
    options: >-
      --health-cmd pg_isready
```

**效果**：

- GitHub Actions runner 启动时顺带起 redis + pg 容器；
- runner job 容器通过 `localhost:6379` / `localhost:5432` 直连（共享网络栈）；
- 带 healthcheck，不用 test 里 sleep 等；
- 每 job 用独立 fresh instance，天然隔离。

### 6.4 CI 缺失项（后续补）

- [ ] **semantic-release** / changelog 自动生成；
- [ ] **镜像 push** 到 registry（当前只 build 未推）；
- [ ] **K8s 清单 dry-run**（`kubectl apply --dry-run`）；
- [ ] **benchmark 对比**（前后 commit）；
- [ ] **安全扫描**（govulncheck / trivy）；
- [ ] **SBOM 生成**（CycloneDX / SPDX）；
- [ ] **多平台镜像** (amd64 + arm64 via buildx)。

---

## 7. Makefile —— 开发者日常命令总表

| Target | 作用 |
|---|---|
| `build` | `go build` 带 version ldflags → `bin/code-agent` |
| `run` | `build` + 直接跑 |
| `test` | `go test -race -cover ./...` |
| `test-short` | `-short` 跳过需要外部依赖的测试 |
| `test-cover` | coverage report + HTML |
| `lint` | `golangci-lint run ./...` |
| `migrate` | 启动 agent（schema 自动 migrate） |
| `docker-build` | `docker build -t code-agent:$VERSION .` |
| `docker-up` | `docker compose up -d` |
| `docker-down` | `docker compose down -v` |
| `clean` | `rm bin/ coverage.out`，`go clean` |
| `tidy` | `go mod tidy` |
| `generate` | `go generate ./...` |
| `openapi` | 打印 `api/openapi.yaml` 路径 |
| `help` | 打印所有 target |

**版本变量注入**：

```makefile
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"
```

**`--dirty` 是关键**：本地有未提交修改，version 会带 `-dirty` 后缀 → 健康检查里一眼看出是不是干净构建。

---

## 8. 四种形态对比总表

| 维度 | Local Dev (`make run`) | Compose (`make docker-up`) | AllInOne (`docker run`) | K8s (`kubectl apply`) |
|---|---|---|---|---|
| **启动时间** | 秒级 | ~30s | ~60s (冷启) | 30s/pod |
| **内存** | ~150MB | ~1.5GB (6 服务) | ~2GB (6 服务同 pod) | 可控 (resources) |
| **数据持久化** | 本地文件 | 命名卷 | VOLUME（易丢） | PV/PVC |
| **扩展性** | 单机单实例 | 单机单实例 | 单机单实例 | HPA 2~10 |
| **HA** | ❌ | ❌ | ❌ | ✅ |
| **Secret 管理** | 配置文件 | env | env | K8s Secret |
| **sandbox 支持** | 宿主 Docker | 挂 sock | DinD (--privileged) | DinD / sysbox / gVisor |
| **适用场景** | 开发者本机 | 团队 demo / 预发 | 销售 demo / 一键体验 | 生产 |

---

## 9. 设计权衡

| 抉择 | 动机 |
|---|---|
| **多阶段 Dockerfile** | builder 800MB+ 降到 runtime <30MB；不把 toolchain 带到生产 |
| **Alpine 而非 distroless** | distroless 更小，但没 shell，排障困难；Alpine 折中 |
| **Go 1.25 (构建时) + Go 1.22 (CI)** | 构建时追新特性；CI 保守确保兼容 |
| **非 root USER** | 满足 PSP / OPA Gatekeeper；减缓逃逸影响 |
| **单 AllInOne vs 多容器 Compose** | AllInOne 适合 demo / 无网络；Compose 适合开发（独立重启） |
| **entrypoint.sh 软降级** | 任一 service down 不致全挂；容错 demo 友好 |
| **PG 初始化分构建 + 启动** | 首次启动秒级；schema 变化时幂等 |
| **DinD 双策略 fuse-overlayfs/vfs** | 内核模块可能缺；vfs 慢但总能跑 |
| `depends_on: service_healthy` | 启动顺序正确；不依赖 sleep |
| **镜像源** `dockerhub.icu / daocloud.io` | 国内 GFW 可靠性 |
| **Viper `AutomaticEnv` + compose env 覆盖** | 一份 yaml，多环境切换靠 env |
| **Docker socket 挂载 vs DinD** | 挂载简单但共享宿主 daemon；DinD 隔离但需 `--privileged` |
| K8s **requests ≪ limits** | 突发容忍 + 装箱效率 |
| `readinessProbe` **独立** | 启动慢时不重启，只摘流量 |
| **HPA 多指标 OR 扩** | 任一超就扩；保守倾向 |
| ConfigMap **readOnly 挂载** | 不可变基础设施；防容器内改配置 |
| CI **sidecar 起 redis/pg** | 测试环境干净；不污染 runner |
| CI **lint/test 为 build/docker 前置** | fail-fast；省 CI 分钟数 |
| CI **docker-build 只在 main** | 保护正式镜像流水线；PR 只验证能否构建 |
| **Makefile `--dirty` version 标记** | 本地脏构建能自识别；健康检查显示 |
| **`.dockerignore` 精简** | `bin/`, `.git/`, `node_modules/` 等不进镜像 |

---

## 10. 后续演进（部署运维方向）

- [ ] **Helm Chart** 打包 K8s 清单；value.yaml 多环境分离；
- [ ] **Kustomize overlays**（dev/staging/prod）；
- [ ] **ArgoCD / FluxCD** GitOps 部署；
- [ ] **镜像多架构** buildx amd64+arm64；
- [ ] **SBOM + 供应链签名** cosign / syft；
- [ ] **K8s `PodSecurityContext`** 完整配置 (`runAsNonRoot`, `readOnlyRootFilesystem`, `capabilities.drop: [ALL]`)；
- [ ] **`NetworkPolicy`** 限定出入网；
- [ ] **`ServiceMonitor`** Prometheus Operator 抓 `/metrics`；
- [ ] **`PrometheusRule`** 告警规则（P99 / error rate / LLM down）；
- [ ] **Grafana Dashboard JSON** 预置；
- [ ] **CI 增加**：govulncheck / trivy 安全扫描；
- [ ] **镜像 push + sign** 到私有 registry；
- [ ] **蓝绿 / 金丝雀发布** via Argo Rollouts；
- [ ] **FinOps**：CPU/内存使用率 → 自动 rightsizing；
- [ ] **Sysbox / gVisor** 沙箱替代 DinD（安全性 +10）；
- [ ] **多租户隔离**：namespace per tenant + quota；
- [ ] **灾难演练** chaos-mesh（杀 pod / 断网） 脚本；
- [ ] **Backup/Restore** PG + Qdrant 定时快照；
- [ ] **在线 migrate 工具**：不重启 agent 的情况下 `goose up`；
- [ ] **`/debug/pprof` 按需暴露**：prod 默认关，`?token=xxx` 才开。

---

## 11. 快速上手 Cheat Sheet

```bash
# ───── Local Dev ─────
make build                                    # 编译
make run                                      # 启动 (需 redis/pg 已起)
make test-short                               # 跑单测

# ───── Compose Demo ─────
make docker-up                                # 起全套
docker compose logs -f agent
make docker-down                              # 关（保留数据）
make docker-down  # 配合 -v 参数会清数据

# ───── AllInOne ─────
bash deploy/download_jaeger.sh                # 下载 jaeger binary
GOOS=linux GOARCH=arm64 go build ...          # 交叉编译
docker build -f Dockerfile.allinone -t code-agent:allinone .
docker run -d --privileged -p 8080:8080 --name ca-allinone code-agent:allinone
open http://localhost:16686                   # Jaeger UI
open http://localhost:8080/healthz            # Agent ready?

# ───── K8s ─────
kubectl create configmap code-agent-config --from-file=configs/
kubectl create secret generic code-agent-secrets --from-literal=openai-api-key=sk-...
kubectl apply -f deployments/k8s/deployment.yaml
kubectl get hpa code-agent-hpa -w
kubectl logs -l app=code-agent -f
```

---

## 11. 实现剖析与改进方向

### Dockerfile 层缓存实战

**好的分层**（本项目已采用）：
```dockerfile
COPY go.mod go.sum ./           ← 单独一层
RUN go mod download              ← dep 变了才失效

COPY . .                         ← 源码
RUN go build ...                 ← 重新编译
```

**坏的分层**（反例）：
```dockerfile
COPY . .                         ← 改任何文件都失效
RUN go mod download              ← 每次都重下
RUN go build ...
```

实测：好的分层下，业务代码改动 rebuild ~20s（只编译）；坏的分层 ~3min
（重下 deps + 编译）。

### K8s 生产清单（`deployments/k8s/deployment.yaml`）

关键字段 checklist：
- [x] `livenessProbe: /healthz`  延迟 10s，每 10s 一次
- [x] `readinessProbe: /readyz`  延迟 5s，每 5s 一次
- [x] `resources.requests.memory: 512Mi`（HPA 基础）
- [x] `resources.limits.memory: 2Gi`（防被 evicted）
- [x] `ServiceAccount` 绑定最小权限
- [x] `Secrets` 注入 LLM API key（非 ConfigMap）
- [ ] `PodDisruptionBudget: minAvailable=1`（升级时不全挂）
- [ ] `topologySpreadConstraints`（Pod 分散不同 AZ）
- [ ] `HPA` 自定义指标（queue depth 而非仅 CPU）

### Pros
- ✅ 多阶段 Dockerfile 瘦身（30MB vs 1GB）
- ✅ 非 root 运行
- ✅ docker-compose.test.yml 独立测试栈
- ✅ CI 流水线：lint → test → build → docker
- ✅ allinone 镜像方便演示

### Cons
- ⚠️ Dockerfile 依赖 `docker.m.daocloud.io` 镜像（CN 友好但国外跑不了）
- ⚠️ K8s 清单缺 PDB / affinity
- ⚠️ 没有 Helm Chart（K8s 部署只能 apply yaml）
- ⚠️ CI 没有 e2e 测试阶段
- ⚠️ AllInOne 镜像体积 500MB+，启动 30s+

### 改进方向
- **P0** — Dockerfile 改用 `ARG GOPROXY`，默认中国镜像，CI 可覆盖
- **P1** — 写 Helm Chart（values.yaml 映射 configs/）
- **P1** — CI 加 e2e（spin up compose → run test_integration.sh）
- **P2** — distroless base image（final 10MB）
- **P2** — 多架构构建（arm64 + amd64）

---

下一篇（可选）：`21_conclusion.md` —— 架构全览回顾、设计哲学总结、新人 Onboarding Checklist。
