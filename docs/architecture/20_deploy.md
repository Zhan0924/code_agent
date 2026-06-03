# 20 · 部署 & 工程化 · `Dockerfile` / `docker-compose.yml` / `Makefile` / k8s

> **范围**：从源码到 4 种运行形态的完整工程链路。本章不重复 18/19 的安全 / 可观测细节，专注**构建产物形态、运行时拓扑、健康探针接线、镜像与配置分离的安全约束**。
>
> **核心代码**：
> - `Dockerfile` (47 行) — 生产多阶段构建
> - `Dockerfile.allinone` (104 行) — 单镜像全栈（PG/Redis/Qdrant/Temporal/Jaeger/DinD）
> - `Dockerfile.local` (26 行) — 静态二进制 + alpine:local 离线构建
> - `Dockerfile.test` (33 行) — 容器内跑单元测试
> - `Dockerfile.p0test` (67 行) — P0 优化项集成验证镜像（含 -race / -bench）
> - `docker-compose.yml` (153 行) — 7 服务本地编排
> - `docker-compose.test.yml` (47 行) — 烟雾测试最小栈
> - `deploy/entrypoint.sh` (162 行) — allinone 镜像 7 阶段启动脚本
> - `deploy/redis.conf` / `deploy/qdrant.yaml` — 嵌入式服务配置
> - `deployments/k8s/deployment.yaml` (121 行) — Deployment + Service + HPA + ServiceAccount + Secret
> - `Makefile` (87 行) — 14 个 target
> - `.dockerignore` — 镜像构建上下文白名单
> - `test_integration.sh` / `test_chat.sh` / `test_comprehensive.sh` / `test_docker.sh` — 集成测试脚本

---

## 1. 模块定位

回答两个工程化问题：

1. **"如何把一个 Go 二进制变成可运行的系统？"** —— 镜像、依赖、配置、健康探针、Sandbox/Docker 接入。
2. **"如何把同一份代码部署到 4 种环境？"** —— 本地开发、单机 Demo、AllInOne 一键体验、K8s 生产。

四种目标形态：

| 形态 | 用例 | 主镜像 | 启动方式 | 是否含外部依赖镜像 |
|---|---|---|---|---|
| **Local Dev** | 开发者本机 | `Dockerfile.local`（可选）| `make run` | 否（依赖容器外起） |
| **Compose Demo** | 单机演示 / 小团队共享 | `Dockerfile` + `docker-compose.yml` | `make docker-up` | 是（7 个服务容器） |
| **AllInOne** | 一键体验 / 销售 Demo / 无网络环境 | `Dockerfile.allinone` | `docker run --privileged ...` | **同一镜像内** 6 服务 |
| **K8s Prod** | 生产、多副本、HPA | `Dockerfile` + `deployments/k8s/deployment.yaml` | `kubectl apply -f` | 否（依赖外部托管服务） |

**贯穿全章的两条主线**：
- ⚠️ **镜像 vs 配置分离**：`.dockerignore` 排除 `configs/config.yaml` 与 `configs/config.allinone.yaml`，避免把密钥烤进镜像。但 `Dockerfile.allinone:79` 仍 `COPY configs/config.allinone.yaml ...` —— 二者直接冲突，构建会失败（详见 §7 DEP-1）。
- ⚠️ **CGO_ENABLED=0 的副作用**：所有生产镜像走纯静态二进制，方便分发，但 **Tree-sitter 失去 C 后端，回退到 regex 模式**（详见 24_treesitter.md 中 TS-2）。

---

## 2. 设计哲学

### 2.1 「构建器 ≠ 运行时」（多阶段）

生产 `Dockerfile` 是 builder + runtime 两阶段：

```text
golang:1.25-alpine  (含 Go 工具链 ~700MB)
   │
   │  go build -ldflags="-w -s" → code-agent (~30MB 静态二进制)
   ▼
alpine:3.20  (基础 ~5MB)
   + ca-certificates + tzdata + curl + git + bash  (~15MB)
   + code-agent 二进制                              (~30MB)
   ────────────────────────────────────────────────
   最终镜像 ~50MB
```

`-ldflags="-w -s"` 去掉符号表与调试信息；运行时镜像不带 Go 工具链 —— 攻击面最小化。

### 2.2 「同源构建，多形态打包」

| 形态 | 构建产物 | 装配方式 |
|---|---|---|
| Dockerfile | builder 阶段调 `go build` | 二进制由 Docker 构建 |
| Dockerfile.local | 宿主机 `go build` → `bin/code-agent-linux` | COPY 静态二进制，**不联网** |
| Dockerfile.allinone | 宿主机 `go build` → `bin/code-agent-linux-arm64` | COPY 二进制 + 嵌入式服务 |
| Dockerfile.test | builder 同源 | `go test` 而非 `go build` |
| Dockerfile.p0test | builder 同源 + CGO=1 | `go vet` + `go test -race` + `go test -bench` |

**统一构建命令**：`CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o bin/code-agent ./cmd/agent`。

### 2.3 「生产可重启 vs 单镜像永驻」

| 形态 | 进程管理 | PID 1 | 失败语义 |
|---|---|---|---|
| Dockerfile | `ENTRYPOINT ["code-agent"]` | code-agent 本体 | 退出即容器退出，由 Docker 重启 |
| Dockerfile.allinone | `ENTRYPOINT ["/entrypoint.sh"]` + `exec code-agent` | code-agent 本体（其他服务为子进程）| 嵌入服务挂掉**不会**重启（entrypoint.sh 仅启动，不 watch） |

AllInOne 的进程模型其实是**伪 supervisor** —— 它启动 6 个后台进程然后 `exec` Agent 顶替自己。Redis/PG/Qdrant 任一挂掉，Agent 还在，但用户难以察觉。仅适合 Demo，**严禁用于生产**。

### 2.4 「不再可见的网络」（CN 镜像源）

| 资源 | 上游 | 镜像 |
|---|---|---|
| Docker Hub | `docker.io/library/*` | `docker.m.daocloud.io/library/*` |
| Docker Hub（备）| `docker.io/library/*` | `docker.1ms.run/library/*` |
| Go 模块代理 | `proxy.golang.org` | `https://goproxy.cn,direct` |
| Go 校验和 DB | `sum.golang.org` | `sum.golang.google.cn` 或 `GOSUMDB=off` |

所有 Dockerfile / docker-compose / Makefile 都内置了这些镜像 —— **离开 CN 网络环境前请整体替换**。

---

## 3. 依赖架构

### 3.1 构建期依赖图

```text
开发者 / CI
   │
   ├── make build  ──────►  go.mod → go modules (via goproxy.cn) → bin/code-agent
   │
   ├── make docker-build ─►  Dockerfile (multi-stage)
   │      ├── stage 1: golang:1.25-alpine (docker.m.daocloud.io)
   │      │   ├── go mod download
   │      │   └── go build → /build/bin/code-agent
   │      └── stage 2: alpine:3.20
   │          ├── apk add: ca-certificates / tzdata / curl / git / bash
   │          └── COPY /build/bin/code-agent → /usr/local/bin/code-agent
   │
   └── make docker-up ────►  docker compose up -d
          ├── agent (build from . using Dockerfile)
          ├── redis:7-alpine
          ├── pgvector/pgvector:pg16
          ├── qdrant/qdrant:v1.12.4
          ├── jaegertracing/all-in-one:latest
          ├── temporalio/auto-setup:1.25
          └── temporalio/ui:2.31.2
```

### 3.2 运行时依赖与端口

| 服务 | 容器名 | 默认端口（host:container） | 镜像（带 CN mirror） |
|---|---|---|---|
| code-agent | `code-agent` | `18080:8080` (HTTP), `18081:8081` (WS/SSE) | 本地构建 |
| Redis | `agent-redis` | `6379:6379` | `docker.m.daocloud.io/library/redis:7-alpine` |
| Postgres | `agent-postgres` | `5432:5432` | `docker.m.daocloud.io/pgvector/pgvector:pg16` |
| Qdrant | `agent-qdrant` | `6333:6333` (HTTP), `6334:6334` (gRPC) | `docker.m.daocloud.io/qdrant/qdrant:v1.12.4` |
| Jaeger | `agent-jaeger` | `16686:16686` (UI), `4317:4317` (OTLP gRPC), `4318:4318` (OTLP HTTP) | `docker.m.daocloud.io/jaegertracing/all-in-one:latest` |
| Temporal | `agent-temporal` | `7233:7233` | `docker.m.daocloud.io/temporalio/auto-setup:1.25` |
| Temporal UI | `agent-temporal-ui` | `8088:8080` | `docker.m.daocloud.io/temporalio/ui:2.31.2` |

### 3.3 启动依赖图（`docker-compose.yml:28-37`）

```text
agent
  ├── depends_on (service_healthy)
  │     ├── redis    ── healthcheck: redis-cli ping
  │     └── postgres ── healthcheck: pg_isready -U agent -d code_agent
  ├── depends_on (service_started)
  │     └── qdrant   ── 无 healthcheck
  └── （temporal/jaeger 未声明依赖，启动竞态由 main.go 的 Warn 降级吸收）

temporal
  └── depends_on (service_healthy) → postgres

temporal-ui
  └── depends_on (default = service_started) → temporal
```

`service_healthy` 保证依赖**健康**才启动，`service_started` 只保证依赖**已启动**（可能还未就绪）。Redis/Postgres 是 Agent 的强依赖（main.go 在 Redis 失败时直接 Fatal），Qdrant 是可选降级（main.go 仅 Warn），因此用 `service_started` 即可。

### 3.4 docker.sock 挂载链路

```text
host:/var/run/docker.sock
        │ (bind mount RW)
        ▼
container:/var/run/docker.sock   ← docker-compose.yml:26
        │
        │ Sandbox.Manager 通过 unix://... 调用
        ▼
internal/sandbox.Manager  →  Docker daemon (host)
```

这意味着 Agent 容器具有**对宿主机 Docker 守护进程的完全控制权**（可创建特权容器、可逃逸）。安全等价于 root，因此 `docker-compose.yml:39` 强制 `user: "0:0"`。详见 §6 安全权衡。

---

## 4. 数据流总览

### 4.1 「从 git push 到生产 Pod」端到端

```text
   开发者
     │
     │ git push origin master
     ▼
   GitHub PR（触发 .github/workflows/pr-review.yml）
     │
     │ Claude PR Review（语义评审，非构建）
     ▼
   合并 → master 分支
     │
     │ ⚠️ 无 CI 自动构建 / 自动推送镜像（详见 §7 DEP-2）
     ▼
   开发者本机：make docker-build
     │
     ├── 多阶段构建（golang:1.25-alpine → alpine:3.20）
     ├── -ldflags 注入 Version + BuildTime（来自 git describe）
     ▼
   code-agent:v0.1.0-xxx 镜像（~50 MB）
     │
     │ docker tag + docker push（手工）
     ▼
   私有 registry / harbor
     │
     │ kubectl set image deployment/code-agent ...
     ▼
   K8s Deployment 滚动更新
     │
     ├── readinessProbe: GET /readyz （5s 间隔，10s 起步延迟）
     ├── livenessProbe:  GET /healthz（30s 间隔，10s 起步延迟）
     └── 新 Pod 就绪后旧 Pod 终止
```

### 4.2 docker-compose 启动时序

```text
T+0s   docker compose up -d
         │
         ├── redis           启动 → healthcheck 每 10s 一次 PING
         ├── postgres        启动 → healthcheck 每 10s 一次 pg_isready
         ├── qdrant          启动（无 healthcheck）
         └── jaeger          启动
T+10s  redis healthy
T+20s  postgres healthy
T+25s  temporal 启动（依赖 postgres healthy）
T+30s  temporal-ui 启动（依赖 temporal started）
T+35s  agent 启动（依赖 redis + postgres healthy + qdrant started）
         │
         ├── main.go 加载 config.yaml
         ├── Ping Redis → 必须成功，否则 Fatal
         ├── 装配 Qdrant client → 失败仅 Warn
         ├── 装配 Sandbox → 通过 /var/run/docker.sock 连 host Docker
         ├── 装配 MCP/Temporal/Skill/Indexer → 各自 Warn 降级
         ├── 注册 25 个 Prometheus metrics
         ├── 启动 HTTP server (:8080) / WS server (:8081)
         └── HEALTHCHECK 每 30s 一次 curl /healthz
T+45s  agent healthy（Docker 标记容器 healthy）
```

### 4.3 AllInOne 启动时序（`deploy/entrypoint.sh`）

```text
T+0s    entrypoint.sh 开始
[1/7]   T+0s ─ T+5s   PostgreSQL: pg_ctl start + 创建 user / db
[2/7]   T+5s ─ T+8s   Redis: redis-server --daemonize（等 30 次 0.5s PING）
[3/7]   T+8s ─ T+15s  Qdrant: 后台启动 + curl /healthz 探测（最多 30 次 1s）
[4/7]   T+15s ─ T+25s Temporal: server start-dev --headless（最多 30 次 1s）
[5/7]   T+25s ─ T+30s Jaeger: badger storage + collector.otlp.grpc(:4317)
[6/7]   T+30s ─ T+35s Docker: 检测 host socket；否则 dockerd --storage-driver fuse-overlayfs/vfs
[7/7]   T+35s         exec code-agent --config /etc/code-agent/configs/config.yaml
                      （Agent 接管 PID 1，前 6 服务为后台子进程，**无 watchdog**）
```

每一步**失败仅打印 ⚠ 警告并继续** —— 镜像内任一服务挂掉，Agent 仍会启动（但相应功能降级）。

### 4.4 健康探针拓扑（与 17_api 对接）

```text
┌────────────────────────────────────────────────────────┐
│ Docker HEALTHCHECK / K8s livenessProbe                  │
│   GET /healthz                                          │
│   → handlers.HealthCheck (api/handlers.go)              │
│   → 200 {"status":"ok"} 总是返回（仅证明进程活着）        │
└────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────┐
│ K8s readinessProbe                                      │
│   GET /readyz                                           │
│   → handlers.ReadinessCheck                             │
│   → Ping Redis + Ping LLM + Ping Qdrant                 │
│   → 200 {"status":"ready"} 仅当核心依赖全部健康          │
│   → 503 {"reason":"redis ping failed"} 否则             │
└────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────┐
│ Prometheus 抓取                                         │
│   GET /metrics                                          │
│   → promhttp.Handler（19_observability.md §5）          │
└────────────────────────────────────────────────────────┘
```

K8s 部署中：`livenessProbe` 用 `/healthz`（**只关心进程是否卡死，不应因下游故障而重启 Pod**）；`readinessProbe` 用 `/readyz`（**下游不健康就摘流量**）。这种 liveness/readiness 分离是 17_api.md §6 反复强调的设计。

---

## 5. 实现细节

### 5.1 `Dockerfile` 逐行解读

```dockerfile
FROM docker.m.daocloud.io/library/golang:1.25-alpine AS builder
                                                       # ↑ 多阶段命名
RUN apk add --no-cache git ca-certificates tzdata      # git 用于 git describe
ENV GOPROXY=https://goproxy.cn,direct                  # CN 网络环境
ENV GOSUMDB=off                                        # 关闭校验和（可改为 sum.golang.google.cn）

WORKDIR /build
COPY go.mod go.sum ./                                  # ★ 先 copy 元数据
RUN go mod download                                    # ★ 再 download，利用 cache 层

COPY . .                                               # 然后才 copy 源码
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s -X main.Version=$(git describe ...)" \
    -o /build/bin/code-agent \
    ./cmd/agent                                        # 不指定 GOARCH，跟随 builder 架构

# ─── 第二阶段 ───
FROM docker.m.daocloud.io/library/alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates tzdata curl git bash && \
    addgroup -S agent && adduser -S agent -G agent     # 非 root 用户

COPY --from=builder /build/bin/code-agent /usr/local/bin/code-agent
COPY --from=builder /build/configs/config.example.yaml /etc/code-agent/configs/config.example.yaml
                                                       # ⚠️ 只 copy example，真实 config.yaml 由 docker-compose mount
USER agent                                             # 切换到非 root
WORKDIR /home/agent
EXPOSE 8080 8081

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1

ENTRYPOINT ["code-agent"]
CMD ["--config", "/etc/code-agent/configs/config.yaml"]
```

**关键点**：
1. `git` 之所以装在运行时镜像，是因为 orchestrator 的 `git_tools.go` 需要执行 git 命令。
2. `bash` 装在运行时镜像，是因为 PTY 模式默认 shell（详见 26_pty.md）。最近一次提交 `1870fc9 fix: Dockerfile 运行时镜像安装 bash` 就是补此漏洞。
3. `curl` 装在运行时镜像，仅为 HEALTHCHECK 用 —— 实际可换成 `wget`（Dockerfile.local 就这么做了）。
4. **`USER agent` 后无法访问 host docker.sock**（docker.sock 默认 root:root 0660）。`docker-compose.yml:39` 通过 `user: "0:0"` 强制覆盖回 root 来解决。

### 5.2 `Dockerfile.allinone` 设计

**总览**：把 PG/Redis/Qdrant/Temporal/Jaeger/Docker 全部 `apk add` 到同一镜像，由 entrypoint.sh 启动。

```dockerfile
FROM alpine:local                                      # ⚠️ 需先 docker tag 一个本地基础镜像
RUN apk add --no-cache redis postgresql16 docker go python3 nodejs npm \
    git curl bash su-exec iptables \
    && mkdir -p /var/lib/postgresql/data /var/lib/redis /var/lib/qdrant/storage

# 初始化 PG 数据目录（构建期，不是运行期）
RUN su-exec postgres initdb -D /var/lib/postgresql/data \
    && echo "host all all 0.0.0.0/0 md5" >> /var/lib/postgresql/data/pg_hba.conf

# 构建期启动 PG 创建 user/db，然后停止
RUN su-exec postgres pg_ctl start -w \
    && su-exec postgres psql -c "CREATE USER agent WITH PASSWORD 'agent_secret' CREATEDB;" \
    && su-exec postgres psql -c "CREATE DATABASE code_agent OWNER agent;" \
    && su-exec postgres pg_ctl stop -w

# COPY 各种二进制 + 配置
COPY deploy/redis.conf /etc/redis/redis.conf
COPY deploy/bin/qdrant /usr/local/bin/qdrant            # 必须预先 download_jaeger.sh 下载
COPY deploy/qdrant.yaml /etc/qdrant/config.yaml
COPY deploy/bin/temporal /usr/local/bin/temporal
COPY deploy/bin/jaeger-all-in-one /usr/local/bin/jaeger-all-in-one
COPY bin/code-agent-linux-arm64 /usr/local/bin/code-agent
COPY configs/config.allinone.yaml /etc/code-agent/configs/config.yaml  # ⚠️ 见 §7 DEP-1
COPY deploy/entrypoint.sh /entrypoint.sh

EXPOSE 8080 6379 5432 6333 6334 7233 4317 16686
VOLUME ["/var/lib/postgresql/data", "/var/lib/redis", "/var/lib/qdrant"]
HEALTHCHECK --interval=15s ... CMD wget -q -O /dev/null http://localhost:8080/healthz
ENTRYPOINT ["/entrypoint.sh"]
```

**陷阱**：
- `alpine:local` —— 用户必须先 `docker tag docker.m.daocloud.io/library/alpine:3.20 alpine:local`，否则构建失败。
- `bin/code-agent-linux-arm64` —— 仅 ARM64 架构。多架构需先 `docker buildx`。
- `deploy/bin/qdrant` / `temporal` / `jaeger-all-in-one` —— 这些二进制**不在仓库内**，需 `deploy/download_jaeger.sh` 等脚本下载。

### 5.3 `entrypoint.sh` 7 阶段启动器

7 个阶段每个都遵循**「启动 → wait loop → fail-soft」**的统一模板：

```bash
# 第 4 阶段示例：Temporal
temporal server start-dev --port 7233 --headless \
    > "$LOG_DIR/temporal.log" 2>&1 &
TEMPORAL_PID=$!

for i in $(seq 1 30); do
    if curl -sf http://localhost:7233/health >/dev/null 2>&1; then break; fi
    if ! kill -0 $TEMPORAL_PID 2>/dev/null; then
        echo "  ⚠ Temporal failed to start"
        break
    fi
    sleep 1
done

if kill -0 $TEMPORAL_PID 2>/dev/null; then
    echo "  ✓ Temporal ready on port 7233"
else
    echo "  ⚠ Temporal not available — workflow engine will be disabled"
fi
# 注意：失败也不 exit 1，继续往下走
```

**第 6 阶段（Docker daemon）的特殊性**：
- 优先使用挂载的 host `/var/run/docker.sock`（普通容器即可）；
- 否则启动 DinD：
  - 优先 `fuse-overlayfs`（需 fuse 模块）；
  - 回退 `vfs`（性能差但兼容性最高）；
  - 需要 `docker run --privileged`，否则 dockerd 起不来。

**第 7 阶段** `exec code-agent` —— Agent 进程替换 entrypoint.sh，成为容器 PID 1。这意味着：
- ✅ SIGTERM 直接到达 Agent → Agent 走完 graceful shutdown
- ❌ 前 6 个后台进程**没人监管** —— 任何一个挂掉 entrypoint 不知道（参见 §7 DEP-3）

### 5.4 `docker-compose.yml` 关键决策

**`user: "0:0"`**（Line 39）：Agent 容器以 root 运行。原因是 `/var/run/docker.sock` 默认权限是 `srw-rw---- root docker`，需要 root 或 docker group。

```yaml
agent:
  user: "0:0"
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock   # ⚠️ 等同于 host root
```

**风险**：容器内任何 Sandbox.Run 都可启动**特权**容器：
```go
// internal/sandbox.Run 内部
client.ContainerCreate(..., &container.HostConfig{
    Privileged: false,           // 当前为 false（安全）
    NetworkMode: "none",          // 当前为 none（更安全，详见 18_auth_security AS-?）
    ...
})
```

只要 sandbox.Manager 代码保证 `Privileged: false` + `NetworkMode: "none"`，容器逃逸的攻击面就被压缩。但如果 sandbox 代码被改成 `Privileged: true`，攻击者通过 LLM 注入恶意 tool_call 即可拿到 host root —— **sandbox 是 host root 的唯一守门员**。

**环境变量注入**（Line 14-19）：
```yaml
environment:
  - CODE_AGENT_REDIS_ADDR=redis:6379
  - CODE_AGENT_POSTGRES_DSN=postgres://agent:agent_secret@postgres:5432/code_agent?sslmode=disable
  - CODE_AGENT_QDRANT_ADDR=qdrant:6334
  - CODE_AGENT_SANDBOX_DOCKER_HOST=unix:///var/run/docker.sock
```

Viper 的 `CODE_AGENT_` 前缀机制（详见 01_config.md）把这些覆盖到 `config.yaml` 之上。**这意味着 config.yaml 里的 `redis.addr: localhost:6379` 会被 `CODE_AGENT_REDIS_ADDR=redis:6379` 覆盖** —— 同一个 config.yaml 既能本地跑（localhost），也能 compose 跑（service hostname）。

**NO_PROXY**（Line 21-22）：CN 网络环境普遍配置了 `HTTP_PROXY` 走梯子，但容器内 service-to-service 通信不应走 proxy。`NO_PROXY=redis,postgres,...` 显式排除。

### 5.5 `Dockerfile.local` 离线构建

设计目标：**无网络环境下构建镜像**（如生产专网、客户现场）。

```dockerfile
FROM alpine:local                  # 需先 docker tag 一个本地基础镜像
# 不 apk add：ca-certificates 已在 alpine 基础镜像中
COPY bin/code-agent-linux /usr/local/bin/code-agent   # 二进制由宿主机预编译
COPY configs/config.example.yaml /etc/code-agent/configs/config.example.yaml
HEALTHCHECK ... CMD wget -q -O /dev/null http://localhost:8080/healthz
ENTRYPOINT ["code-agent"]
```

**用法**：
```bash
# 一次性：把可用的 alpine 镜像 tag 为 alpine:local
docker tag docker.1ms.run/alpine:latest alpine:local

# 编译静态二进制（在宿主机）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" \
    -o bin/code-agent-linux ./cmd/agent

# 完全离线构建
docker build --network=none -f Dockerfile.local -t code-agent:test .
```

`--network=none` 强制断网构建 —— 任何 `apk add` 都会失败，所以镜像里**不能**有 `RUN apk add`（这就是为什么不装 curl，用 wget 替代）。

### 5.6 `Dockerfile.test` / `Dockerfile.p0test` 容器内测试

**Dockerfile.test**：CI 环境跑指定单元测试。`go build -o /dev/null ./cmd/agent` 仅验证编译，不产出二进制；CMD 跑 `go test -run "TestValidateDAG|TestTopologicalSort|..."` 等指定 P1 模块的用例。

**Dockerfile.p0test**：四阶段 P0 优化项验证镜像
1. `go build` ：编译通过证明 API 兼容；
2. `go vet`：静态检查；
3. `go test -race -v`：带竞态检测的全用例（CGO=1，需 gcc/musl-dev）；
4. `go test -bench`：性能对比（Cache hit/miss 吞吐对比）。

```bash
# 用法
docker build -f Dockerfile.p0test -t p0test:latest .
docker run --rm p0test:latest
```

**为何区分 test/p0test**：
- test 镜像无 -race（更快），仅验证函数行为；
- p0test 镜像带 -race + -bench（更慢），针对 P0 优化项做完整 quality gate。

### 5.7 `.dockerignore` 白名单语义

```text
# Build artifacts
# bin/ — NOT excluded: Dockerfile.local needs bin/code-agent-linux
coverage.out

# IDE / Git / CI / Docker self-reference
.idea/  .vscode/  *.swp  .git/  .gitignore  .github/  docker-compose.yml  Dockerfile

# ★ 密钥配置（防止烤进镜像）
configs/config.yaml
configs/config.allinone.yaml

# Docs / Test caches / OS files
README.md  *.md  **/*_test.go  .DS_Store  Thumbs.db
```

**关键设计**：
- `configs/config.yaml` 与 `configs/config.allinone.yaml` **被排除** —— 防止任何包含真实 LLM API key、PG 密码的配置烤进可分发镜像。
- `bin/` **不**排除 —— Dockerfile.local 依赖 `bin/code-agent-linux`。
- `**/*_test.go` 排除 —— 测试代码不进生产镜像（但 Dockerfile.test/p0test 用单独的 `.dockerignore.test` 覆盖此规则）。

### 5.8 `Makefile` 14 个 target

```text
build         go build $(LDFLAGS) -o bin/code-agent ./cmd/agent
run           build + ./bin/code-agent
test          go test -race -cover ./...
test-short    go test -short -race ./...                    # 跳过需要外部服务的测试
test-cover    -coverprofile + tail 总行
lint          golangci-lint run ./...
migrate       仅说明 "auto-applied on startup"
docker-build  docker build -t code-agent:$(VERSION) .
docker-up     docker compose up -d
docker-down   docker compose down -v                         # ⚠️ -v 删卷
clean         rm -rf bin/ coverage.out
tidy          go mod tidy
generate      go generate ./...
openapi       cat api/openapi.yaml | head -5                 # 仅显示路径
help          打印所有 target 说明
```

**`LDFLAGS` 注入版本**（Line 4-6）：
```makefile
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"
```

→ 运行时通过 `/api/v1/version` 端点暴露（详见 17_api.md）。

### 5.9 K8s 部署清单

`deployments/k8s/deployment.yaml` 包含 5 个资源：

| 资源 | 名称 | 关键配置 |
|---|---|---|
| Deployment | `code-agent` | `replicas: 2`，2 副本起步；资源 `cpu 250m–1000m`，`memory 256Mi–512Mi` |
| Service | `code-agent` | ClusterIP，仅集群内可达 |
| HorizontalPodAutoscaler | `code-agent-hpa` | min 2 / max 10 副本，CPU 70%、Memory 80% |
| ServiceAccount | `code-agent` | 占位，未绑定 Role |
| Secret | `code-agent-secrets` | `stringData.openai-api-key: your-api-key-here` ⚠️ **明文占位符** |

**关键探针配置**（Line 47-58）：
```yaml
livenessProbe:
  httpGet: { path: /healthz, port: http }
  initialDelaySeconds: 10
  periodSeconds: 30
readinessProbe:
  httpGet: { path: /readyz, port: http }
  initialDelaySeconds: 5
  periodSeconds: 10
```

- `livenessProbe` 30s 间隔避免误杀（ReAct 循环可能 60s+ 不响应业务请求，但 /healthz 永远秒返）；
- `readinessProbe` 10s 间隔可较快摘流量。

**多副本约束**（与 18_auth_security AS-1 联动）：
```yaml
# ⚠️ Deployment 必须注入 CODE_AGENT_AUTH_JWT_SECRET secret，否则每个 Pod
# 的 JWT 签名密钥独立随机生成，跨 Pod 的 token 互相验签失败 → 用户登出。
env:
  - name: CODE_AGENT_AUTH_JWT_SECRET
    valueFrom:
      secretKeyRef: { name: code-agent-secrets, key: jwt-secret }
```

**当前 deployment.yaml 未配置 JWT secret env**，这是已知缺陷 DEP-4。

### 5.10 集成测试脚本

| 脚本 | 目标 | 入口镜像 |
|---|---|---|
| `test_integration.sh` | api 包的 integration_test.go（miniredis + zap observer） | `docker.1ms.run/golang:1.24-alpine` |
| `test_chat.sh` | 通过 curl 调 `/api/v1/chat/react-stream` 烟雾测试 | 宿主机直跑 |
| `test_comprehensive.sh` | 全栈端到端：起 compose → 发请求 → 验日志 | 宿主机直跑 |
| `test_docker.sh` | 仅验证 docker 镜像构建 + 单次启动 | 宿主机直跑 |

`test_integration.sh` 的关键设计：**在 `golang:1.24-alpine` 容器内跑 `go test`**，这样开发者本机的 Go 版本不需要精确匹配，且测试环境与 CI 完全一致。

---

## 6. 设计权衡

### 6.1 多阶段 vs 单阶段

| 方案 | 镜像大小 | 攻击面 | 缓存效率 |
|---|---|---|---|
| 单阶段 `golang:1.25-alpine` | ~700MB | 含 Go 工具链 | 高（一层） |
| **多阶段 builder + runtime** | **~50MB** | **仅运行时依赖** | 中（多层） |

选**多阶段**。代价是 Dockerfile 复杂度↑，但 50MB vs 700MB 的差距在分发场景压倒一切。

### 6.2 docker.sock 挂载 vs DinD

| 方案 | 性能 | 安全 | 兼容性 |
|---|---|---|---|
| **挂载 host docker.sock** | 高（原生 Docker API）| 低（等价 host root）| 受限于 host 内核 |
| DinD（特权容器内跑 dockerd）| 低（双层抽象）| 中（仅容器内逃逸）| --privileged 要求高 |

**docker-compose.yml 选「挂载 host docker.sock」**，因为：
- Demo 环境对性能敏感（Sandbox 启动延迟直接影响用户体验）；
- 用户对 host 有完全控制权（不是多租户）；
- DinD 在某些云环境（如 GCP COS）受限。

**AllInOne 选「DinD 优先 + host socket 降级」**，因为它要支持「真正一键运行」的零配置场景。

### 6.3 嵌入式服务 vs 独立容器

| 维度 | AllInOne（嵌入式）| docker-compose（独立）|
|---|---|---|
| 启动速度 | 慢（35s 7 阶段串行）| 中（compose 并行启动，~35s 但更快进入工作状态） |
| 内存占用 | 高（~1.5GB 单容器）| 中（每服务独立计费）|
| 升级方式 | 重建整个镜像 | 单服务升级 |
| 故障隔离 | 差（Agent 拉不起 → 整个容器死）| 好（service_healthy 隔离）|
| 多副本扩展 | 不可能 | 可（重启 redis 不影响 agent）|
| 生产可用？ | ❌ 仅 Demo | ✅ 可 |

AllInOne 是**纯 Demo / 销售场景**的工具，不应混淆为生产能力。

### 6.4 ENTRYPOINT 形式选择

| Dockerfile | ENTRYPOINT | 优点 | 缺点 |
|---|---|---|---|
| Dockerfile | `["code-agent"]` exec form | 信号直传，PID 1 干净 | 启动前无 shell 初始化 |
| Dockerfile.allinone | `["/entrypoint.sh"]` | 多服务编排 | 子进程无 watchdog |
| Dockerfile.local | `["code-agent"]` exec form | 同生产 | — |
| Dockerfile.test | `["go", "test", ...]` | 直接跑测试 | 不是 server |

生产路径用 exec form 是因为 shell form (`code-agent ...` 不加 `[]`) 会被 `/bin/sh -c` 包裹，SIGTERM 只能到达 sh，无法终止 code-agent。

### 6.5 K8s livenessProbe = /healthz 而不是 /readyz

**理由**：`/readyz` 检查下游依赖（Redis/Qdrant），如果用作 liveness：
- Redis 短暂故障 → 所有 Agent Pod 被 K8s 重启 → 雪崩；
- 重启不修复 Redis，反而延长故障时间。

`/healthz` 仅判断进程**是否还活着**（永远 200，除非进程死锁）。下游故障 → readiness 失败 → K8s 摘流量但不重启 Pod → Redis 恢复后 readiness 自动回绿。

### 6.6 镜像源选择

CN 网络环境下：
- ✅ `docker.m.daocloud.io` — 稳定，被多个生产环境验证
- ✅ `docker.1ms.run` — 备用，部分场景更快
- ❌ `dockerhub.icu` — 历史曾用，已不可达（参见 Dockerfile L4 注释）

`goproxy.cn` 是 Go 模块代理的事实标准。

---

## 7. 后续演进 / 设计教训 / 已知缺陷一览

### 7.1 已知缺陷

| ID | 缺陷 | 文件:行 | 严重度 | 修复建议 |
|---|---|---|---|---|
| **DEP-1** | `Dockerfile.allinone:79 COPY configs/config.allinone.yaml` 与 `.dockerignore` 中 `configs/config.allinone.yaml` 互斥 → 构建会失败 | `Dockerfile.allinone:79` ⊕ `.dockerignore:23` | 🔴 P0 | 二选一：要么 `.dockerignore` 添加 `!Dockerfile.allinone` 路径白名单，要么 allinone 改成运行时 bind mount |
| **DEP-2** | 无 `.github/workflows/ci.yml`，仅有 `pr-review.yml`（LLM 评审）。test/build/push 镜像全靠手工 | `.github/workflows/` | 🟠 P1 | 补 CI：go test + docker build + push 到 registry |
| **DEP-3** | `entrypoint.sh` 前 6 服务挂掉无人察觉。e.g. PG 后台死掉，Agent 仍在跑但所有持久化操作 500 | `deploy/entrypoint.sh` | 🟠 P1 | 引入 supervisord 或在 Agent /readyz 里加深度检查 |
| **DEP-4** | `deployments/k8s/deployment.yaml` 未注入 `CODE_AGENT_AUTH_JWT_SECRET` env → 多副本时 JWT 签名密钥独立随机生成，跨 Pod token 互相验签失败 | `deployments/k8s/deployment.yaml:28` | 🔴 P0 | 添加 secretKeyRef 注入 JWT_SECRET，与 18_auth_security AS-1 联动修复 |
| **DEP-5** | `deployments/k8s/deployment.yaml` 中 Secret 包含明文占位符 `your-api-key-here` —— 容易被误 commit 真实密钥 | `deployments/k8s/deployment.yaml:120` | 🟠 P1 | 用 sealed-secrets / external-secrets-operator 替代 |
| **DEP-6** | `docker-compose.yml:39 user: "0:0"` 把 Agent 跑成 root —— sandbox 逃逸即 host root | `docker-compose.yml:39` | 🟠 P1 | 改用 rootless docker 或 user namespaces remap |
| **DEP-7** | `Dockerfile.allinone` 依赖宿主机预下载的 `deploy/bin/{qdrant,temporal,jaeger-all-in-one}`，构建脚本未提供（仅 `download_jaeger.sh`）| `Dockerfile.allinone:63-75` | 🟡 P2 | 补 `deploy/download_all.sh` 一键下载 |
| **DEP-8** | `golangci-lint` 配置 `go: "1.22"`，但 `go.mod` 声明 `go 1.25.0` —— CI 可能版本不匹配 | `.golangci.yml` ⊕ `go.mod` | 🟡 P2 | 统一到 1.25 或在 CLAUDE.md 中文档化 GOTOOLCHAIN=auto |
| **DEP-9** | `docker-compose.yml:53 redis healthcheck` 间隔 10s，启动期 50s 才确认健康 —— Agent 启动延迟过长 | `docker-compose.yml:53-56` | 🟢 P3 | start_period: 5s + start_interval: 1s（compose 2.20+）|
| **DEP-10** | `GOSUMDB=off` 关闭 Go 模块校验和验证 —— 供应链攻击风险 | `Dockerfile:12` | 🟢 P3 | 改 `GOSUMDB=sum.golang.google.cn` |

### 7.2 设计教训

1. **「.dockerignore 与 COPY 顺序」必须互查**：DEP-1 完全可被 `docker build` 报错及早暴露，但 CI 缺失（DEP-2）导致问题潜伏。
2. **「entrypoint = supervisor」是反模式**：Shell 脚本无法做 watchdog 是 entrypoint.sh 7-fail-soft 阶段最大的工程债。AllInOne 想做 Demo 工具，应坦诚承认这点（不要文档里写"production-grade"）。
3. **「JWT secret 多副本一致性」必须在部署清单中显式声明**：DEP-4 是 18_auth_security AS-1 在 k8s 层的镜像 —— 安全设计在代码层是对的（fatal warning），但部署清单层没接住。
4. **「-w -s 去符号 vs 性能 profiling」的取舍**：当前生产镜像去掉了符号表，pprof 仅能拿到地址不能拿到函数名 —— 生产排障需要补 debug 镜像变种。

### 7.3 演进方向

| 优先级 | 项 | 预计工作量 |
|---|---|---|
| P0 | 修 DEP-1 / DEP-4 | 0.5d |
| P0 | 补 GitHub Actions CI（test + docker build + push） | 1d |
| P1 | k8s deployment 增加 PodDisruptionBudget + topologySpreadConstraints | 0.5d |
| P1 | 引入 rootless docker 或 sysbox 替换 `user: "0:0"` | 2d |
| P1 | `entrypoint.sh` 换成 supervisord（仅 AllInOne 形态） | 1d |
| P2 | 多架构镜像（`docker buildx` amd64 + arm64） | 0.5d |
| P2 | 发布 helm chart | 2d |
| P3 | Distroless 运行时镜像（去掉 alpine） | 1d |
| P3 | Bazel/Buck2 替代 Dockerfile（构建可重现性） | 5d+ |

---

## 8. 测试矩阵

| 测试 | 入口 | 范围 | 是否需要外部服务 |
|---|---|---|---|
| `go test -short -race ./...` | `make test-short` | 单元测试（跳过 Redis/Qdrant/Docker 依赖项） | 否 |
| `go test -race -cover ./...` | `make test` | 全量单元 + 集成 | 需 Redis/Qdrant |
| `test_integration.sh` | 宿主机 `./test_integration.sh` | api 包 integration_test.go（miniredis + zap）| 仅需 Docker（容器内跑）|
| `Dockerfile.test` | `docker build -f Dockerfile.test && docker run --rm code-agent-test` | P1 模块（planner/repomap/llm/orchestrator）的精选用例 | 否 |
| `Dockerfile.p0test` | `docker build -f Dockerfile.p0test && docker run --rm p0test:latest` | P0 优化项 4 阶段（build + vet + race test + bench）| 否 |
| `docker-compose.test.yml` | `docker compose -f docker-compose.test.yml up -d` | TestP0_* HTTP 烟雾测试 | 自带最小栈（redis-p0 + qdrant-p0）|
| `test_chat.sh` | 宿主机 `./test_chat.sh` | ReAct chat 端到端 | 需完整 compose 栈 |
| `test_comprehensive.sh` | 宿主机 `./test_comprehensive.sh` | 多端点全功能验证 | 需完整 compose 栈 |
| `test_docker.sh` | 宿主机 `./test_docker.sh` | 仅验证镜像构建 + 启动 | 需 Docker |

**端到端验证（compose 形态）**：
```bash
make docker-up                              # 启动栈
sleep 30                                    # 等待 healthy
curl -f http://localhost:18080/healthz      # 进程活
curl -f http://localhost:18080/readyz       # 依赖就绪
curl -X POST http://localhost:18080/api/v1/chat ... # 业务可用
make docker-down                            # 清理
```

---

## 9. 配置示例

### 9.1 本地开发（推荐）

```bash
# 1. 复制配置模板，填入真实 API key
cp configs/config.example.yaml configs/config.yaml
vim configs/config.yaml

# 2. 起依赖栈（不含 agent，方便本地 go run / dlv 调试）
docker compose up -d redis postgres qdrant jaeger

# 3. 本地跑 agent
make run
# 或带调试器
dlv debug ./cmd/agent -- --config configs/config.yaml
```

### 9.2 单机 Demo（compose）

```bash
# 1. 配置 LLM API key
export CODE_AGENT_LLM_PRIMARY_API_KEY="sk-xxx"

# 2. 起完整栈
make docker-build           # 构建 agent 镜像
make docker-up              # 启动 7 服务

# 3. 验证
curl http://localhost:18080/healthz
curl http://localhost:18080/readyz
open http://localhost:16686  # Jaeger UI
open http://localhost:8088   # Temporal UI

# 4. 清理
make docker-down            # ⚠️ 同时删除卷（数据丢失）
```

### 9.3 AllInOne 一键体验

```bash
# 1. 预下载二进制
./deploy/download_jaeger.sh

# 2. 编译 Linux ARM64 二进制（在 ARM 宿主机或 buildx）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
    -ldflags="-w -s" -o bin/code-agent-linux-arm64 ./cmd/agent

# 3. tag 本地基础镜像
docker tag docker.m.daocloud.io/library/alpine:3.20 alpine:local

# 4. 构建（注意 .dockerignore 冲突，需先修 DEP-1）
docker build -f Dockerfile.allinone -t code-agent:allinone .

# 5. 运行（带 sandbox）
docker run -d --privileged -p 8080:8080 \
    --name code-agent-allinone \
    code-agent:allinone

# 6. 验证
docker logs -f code-agent-allinone
curl http://localhost:8080/healthz
```

### 9.4 K8s 生产部署（修复 DEP-4 后）

```yaml
# secrets.yaml
apiVersion: v1
kind: Secret
metadata:
  name: code-agent-secrets
type: Opaque
stringData:
  openai-api-key: "sk-real-key-here"
  jwt-secret: "openssl rand -hex 32 的输出"        # ← 必须显式指定
  api-key-1: "ak-prod-team-frontend"
---
# deployment-patch.yaml（追加到 deployments/k8s/deployment.yaml）
spec:
  template:
    spec:
      containers:
        - name: code-agent
          env:
            - name: CODE_AGENT_LLM_PRIMARY_API_KEY
              valueFrom: { secretKeyRef: { name: code-agent-secrets, key: openai-api-key } }
            - name: CODE_AGENT_AUTH_JWT_SECRET                            # ★ DEP-4 修复
              valueFrom: { secretKeyRef: { name: code-agent-secrets, key: jwt-secret } }
            - name: CODE_AGENT_AUTH_API_KEYS
              valueFrom: { secretKeyRef: { name: code-agent-secrets, key: api-key-1 } }
            - name: CODE_AGENT_REDIS_ADDR
              value: "redis-cluster.default.svc.cluster.local:6379"
            - name: CODE_AGENT_QDRANT_ADDR
              value: "qdrant.default.svc.cluster.local:6334"
            - name: CODE_AGENT_TRACING_ENDPOINT
              value: "otel-collector.observability.svc.cluster.local:4317"
```

```bash
kubectl apply -f secrets.yaml
kubectl apply -f deployments/k8s/deployment.yaml

# 监控滚动更新
kubectl rollout status deployment/code-agent
kubectl logs -l app=code-agent --tail=100 -f
```

### 9.5 离线构建（无网络环境）

```bash
# 在有网络的环境
docker pull docker.1ms.run/alpine:latest
docker save docker.1ms.run/alpine:latest > alpine.tar
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" -o bin/code-agent-linux ./cmd/agent

# 拷贝到离线环境后
docker load < alpine.tar
docker tag docker.1ms.run/alpine:latest alpine:local
docker build --network=none -f Dockerfile.local -t code-agent:test .

# 运行（无 docker-compose，手工起依赖）
docker run -d --name redis -p 6379:6379 redis:7-alpine
docker run -d -p 8080:8080 \
    -v $(pwd)/configs/config.yaml:/etc/code-agent/configs/config.yaml:ro \
    -e CODE_AGENT_REDIS_ADDR=host.docker.internal:6379 \
    code-agent:test
```

---

## 10. 跨文档引用

- **01_config.md** —— Viper `CODE_AGENT_` env 前缀语义，与 docker-compose `environment:` 注入互动。
- **05_sandbox.md** —— 解释 `/var/run/docker.sock` 挂载与 `NetworkMode: "none"` 的 SSRF 防线（与 18 联动）。
- **17_api.md** —— `/healthz` vs `/readyz` 的语义与本章 5.4 / 5.9 的探针配置呼应。
- **18_auth_security.md** —— AS-1（JWT secret 多副本）是本章 DEP-4 的代码层成因。
- **19_observability.md** —— Prometheus `/metrics` 端点 + Jaeger OTLP 4317 端口的拓扑落地在本章 §3.2。
- **24_treesitter.md** —— CGO_ENABLED=0 → tree-sitter regex 回退模式（TS-2）。
- **26_pty.md** —— PTY 模式 `/bin/bash` 默认 shell，是 Dockerfile 运行时镜像必须装 bash 的原因。

---

## 11. 下一篇导引

至此 25 篇 `docs/architecture/` 全部完成：
- `00_overview.md` 总览
- `01_config.md` – `09_orchestrator.md` 配置 / 模型 / LLM / RAG / Sandbox / MCP / Tools / Skill / 编排
- `10_planner.md` – `15_indexer_repomap.md` 规划 / Temporal / Session / Context / Workspace / Indexer
- `16_store.md` – `20_deploy.md` 存储 / API / 安全 / 可观测 / 部署
- `21_agentloop.md` – `28_generator.md` AgentLoop / MultiAgent / ToolLearn / Tree-sitter / Memory / PTY / LSP / Generator

**下一步**：同步 `llmdoc/index.md` 与 `llmdoc/startup.md` 引用 → 让 25 篇文档真正进入「上下文优先」的入口。
