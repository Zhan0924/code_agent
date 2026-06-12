# ============================================================
# Multi-stage build for production-grade Code Agent
# ============================================================
# Use a reachable Docker Hub mirror (dockerhub.icu is currently unavailable).
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

# Use a reachable Go module proxy for builds that don't have access to
# proxy.golang.org (typical in CN-network environments).
ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# GOARCH intentionally unset: the go toolchain picks the builder image's
# architecture, so the binary matches the runtime alpine below on either
# amd64 (CI) or arm64 (Apple Silicon) without needing QEMU emulation.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s -X main.Version=$(git describe --tags --always 2>/dev/null || echo v0.1.0)" \
    -o /build/bin/code-agent \
    ./cmd/agent

# ============================================================
FROM alpine:3.20 AS runtime

# Dev toolchains for LLM-executed workspace commands. The agent runs
# `exec.Command("sh","-c", ...)` directly inside this container
# (internal/orchestrator/file_tools.go::toolRunWorkspaceCmd), so anything the
# LLM types (`go build`, `npm install`, `pytest`, `make`) must resolve here.
# Without this layer, every compiled-language command fails with exit 127.
RUN apk add --no-cache \
        ca-certificates tzdata curl git bash \
        nodejs npm \
        python3 py3-pip \
        build-base && \
    rm -f /usr/lib/python*/EXTERNALLY-MANAGED && \
    addgroup -S agent && adduser -S agent -G agent

# Copy Go 1.25 from the builder stage so user `go build` / `go test` runs the
# same toolchain version the agent itself was compiled with. With
# GOTOOLCHAIN=auto, any go.mod requesting a different version still downloads
# automatically.
COPY --from=builder /usr/local/go /usr/local/go

ENV PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
    GOTOOLCHAIN=auto \
    GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=off \
    GOPATH=/home/agent/go \
    GOCACHE=/home/agent/.cache/go-build \
    GOMODCACHE=/home/agent/go/pkg/mod \
    NPM_CONFIG_REGISTRY=https://registry.npmmirror.com \
    NPM_CONFIG_CACHE=/home/agent/.npm \
    PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple

# Pre-create cache dirs owned by `agent` so first-run go/npm/pip don't fail
# trying to mkdir in HOME without write perms.
RUN mkdir -p /home/agent/go/pkg/mod /home/agent/.cache/go-build /home/agent/.npm && \
    chown -R agent:agent /home/agent

COPY --from=builder /build/bin/code-agent /usr/local/bin/code-agent
# Copy only the example config — real config.yaml is provided at runtime via
# bind mount (see docker-compose.yml). This prevents secrets being baked in.
COPY --from=builder /build/configs/config.example.yaml /etc/code-agent/configs/config.example.yaml

USER agent
WORKDIR /home/agent

EXPOSE 8080 8081

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1

ENTRYPOINT ["code-agent"]
CMD ["--config", "/etc/code-agent/configs/config.yaml"]
