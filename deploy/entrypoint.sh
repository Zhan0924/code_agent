#!/bin/bash
set -e

LOG_DIR="/var/log/agent"
mkdir -p "$LOG_DIR"
chmod 777 "$LOG_DIR"

echo "============================================================"
echo "  Code Agent All-in-One Container Starting (Full Stack)"
echo "  Services: PG · Redis · Qdrant · Temporal · Jaeger · Docker"
echo "============================================================"

# ── 1. Start PostgreSQL ──────────────────────────────────────
echo "[1/7] Starting PostgreSQL..."
chown -R postgres:postgres /var/lib/postgresql/data /run/postgresql 2>/dev/null || true

if [ ! -f /var/lib/postgresql/data/PG_VERSION ]; then
    echo "  → Initializing fresh PostgreSQL data directory..."
    su-exec postgres initdb -D /var/lib/postgresql/data
    echo "host all all 0.0.0.0/0 md5" >> /var/lib/postgresql/data/pg_hba.conf
    echo "listen_addresses='*'" >> /var/lib/postgresql/data/postgresql.conf
fi

su-exec postgres pg_ctl -D /var/lib/postgresql/data start -w -t 30 -l "$LOG_DIR/postgresql.log"

su-exec postgres psql -tc "SELECT 1 FROM pg_roles WHERE rolname='agent'" | grep -q 1 || \
    su-exec postgres psql -c "CREATE USER agent WITH PASSWORD 'agent_secret' CREATEDB;"
su-exec postgres psql -tc "SELECT 1 FROM pg_database WHERE datname='code_agent'" | grep -q 1 || \
    su-exec postgres psql -c "CREATE DATABASE code_agent OWNER agent;"

echo "  ✓ PostgreSQL ready on port 5432"

# ── 2. Start Redis ───────────────────────────────────────────
echo "[2/7] Starting Redis..."
redis-server /etc/redis/redis.conf --daemonize yes --logfile "$LOG_DIR/redis.log"

for i in $(seq 1 30); do
    if redis-cli ping 2>/dev/null | grep -q PONG; then break; fi
    sleep 0.5
done
echo "  ✓ Redis ready on port 6379"

# ── 3. Start Qdrant ─────────────────────────────────────────
echo "[3/7] Starting Qdrant..."
qdrant --config-path /etc/qdrant/config.yaml > "$LOG_DIR/qdrant.log" 2>&1 &
QDRANT_PID=$!

# Wait for Qdrant gRPC to become available
for i in $(seq 1 30); do
    if curl -sf http://localhost:6333/healthz >/dev/null 2>&1; then break; fi
    if ! kill -0 $QDRANT_PID 2>/dev/null; then
        echo "  ⚠ Qdrant failed to start (see $LOG_DIR/qdrant.log)"
        break
    fi
    sleep 1
done

if curl -sf http://localhost:6333/healthz >/dev/null 2>&1; then
    echo "  ✓ Qdrant ready on ports 6333 (HTTP) / 6334 (gRPC)"
else
    echo "  ⚠ Qdrant not available — RAG will be disabled"
fi

# ── 4. Start Temporal Dev Server ─────────────────────────────
echo "[4/7] Starting Temporal (dev server)..."
temporal server start-dev \
    --namespace code-agent \
    --port 7233 \
    --headless \
    --log-format json \
    --log-level warn \
    --db-filename /var/lib/qdrant/temporal.db \
    > "$LOG_DIR/temporal.log" 2>&1 &
TEMPORAL_PID=$!

# Wait for Temporal to become available
for i in $(seq 1 30); do
    if curl -sf http://localhost:7233/health >/dev/null 2>&1; then break; fi
    if ! kill -0 $TEMPORAL_PID 2>/dev/null; then
        echo "  ⚠ Temporal failed to start (see $LOG_DIR/temporal.log)"
        break
    fi
    sleep 1
done

if kill -0 $TEMPORAL_PID 2>/dev/null; then
    echo "  ✓ Temporal ready on port 7233 (namespace: code-agent)"
else
    echo "  ⚠ Temporal not available — workflow engine will be disabled"
fi

# ── 5. Start Jaeger (Distributed Tracing) ────────────────────
echo "[5/7] Starting Jaeger (tracing backend)..."
SPAN_STORAGE_TYPE=badger \
    BADGER_EPHEMERAL=false \
    BADGER_DIRECTORY_KEY=/var/lib/qdrant/jaeger/key \
    BADGER_DIRECTORY_VALUE=/var/lib/qdrant/jaeger/data \
    jaeger-all-in-one \
    --collector.otlp.grpc.host-port=:4317 \
    --collector.otlp.http.host-port=:4318 \
    --query.http-server.host-port=:16686 \
    > "$LOG_DIR/jaeger.log" 2>&1 &
JAEGER_PID=$!
mkdir -p /var/lib/qdrant/jaeger/key /var/lib/qdrant/jaeger/data

for i in $(seq 1 20); do
    if curl -sf http://localhost:16686/ >/dev/null 2>&1; then break; fi
    if ! kill -0 $JAEGER_PID 2>/dev/null; then
        echo "  ⚠ Jaeger failed to start (see $LOG_DIR/jaeger.log)"
        break
    fi
    sleep 1
done

if kill -0 $JAEGER_PID 2>/dev/null; then
    echo "  ✓ Jaeger ready — UI: :16686, OTLP gRPC: :4317"
else
    echo "  ⚠ Jaeger not available — tracing will be disabled"
fi

# ── 6. Start Docker Daemon (DinD for Sandbox) ────────────────
echo "[6/7] Starting Docker daemon (sandbox)..."
if [ -e /var/run/docker.sock ] && docker info >/dev/null 2>&1; then
    # Host Docker socket is mounted — use it directly
    echo "  ✓ Using mounted Docker socket (sandbox enabled)"
elif [ -d /proc/self/ns ]; then
    # Try DinD: prefer fuse-overlayfs, fall back to vfs
    STORAGE_DRIVER="fuse-overlayfs"
    if ! which fuse-overlayfs >/dev/null 2>&1; then
        STORAGE_DRIVER="vfs"
    fi
    dockerd --storage-driver="$STORAGE_DRIVER" \
        --log-level=warn \
        --iptables=true \
        > "$LOG_DIR/dockerd.log" 2>&1 &
    DOCKERD_PID=$!

    for i in $(seq 1 30); do
        if docker info >/dev/null 2>&1; then break; fi
        if ! kill -0 $DOCKERD_PID 2>/dev/null; then
            echo "  ⚠ Docker daemon failed (need --privileged flag)"
            break
        fi
        sleep 1
    done

    if docker info >/dev/null 2>&1; then
        echo "  ✓ Docker daemon ready (sandbox enabled)"
    else
        echo "  ⚠ Docker daemon not available — run with --privileged to enable sandbox"
    fi
else
    echo "  ⚠ Docker socket not found — sandbox disabled (run with --privileged)"
fi

# ── 7. Start Code Agent ─────────────────────────────────────
echo "[7/7] Starting Code Agent..."
echo "  → HTTP API on :8080"
echo "============================================================"

# Run agent in foreground so Docker can track its lifecycle.
exec code-agent --config /etc/code-agent/configs/config.yaml
