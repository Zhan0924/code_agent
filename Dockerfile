# ============================================================
# Multi-stage build for production-grade Code Agent
# ============================================================
# Use a reachable Docker Hub mirror (dockerhub.icu is currently unavailable).
FROM docker.m.daocloud.io/library/golang:1.25-alpine AS builder

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
FROM docker.m.daocloud.io/library/alpine:3.20 AS runtime

RUN apk add --no-cache ca-certificates tzdata curl git bash && \
    addgroup -S agent && adduser -S agent -G agent

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
