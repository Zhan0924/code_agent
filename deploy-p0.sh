#!/bin/bash

# Create network if not exists
podman network inspect p0-net >/dev/null 2>&1 || podman network create p0-net

# 1. Redis
echo "Starting Redis..."
podman run -d --name redis --network p0-net -p 16379:6379 \
    docker.io/library/redis:7-alpine \
    redis-server --appendonly yes --maxmemory 256mb --maxmemory-policy allkeys-lru

# 2. Postgres (pgvector)
echo "Starting Postgres..."
podman run -d --name postgres --network p0-net -p 15432:5432 \
    -e POSTGRES_DB=code_agent \
    -e POSTGRES_USER=agent \
    -e POSTGRES_PASSWORD=agent_secret \
    docker.m.daocloud.io/pgvector/pgvector:pg16

# 3. Qdrant
echo "Starting Qdrant..."
podman run -d --name qdrant --network p0-net -p 6333:6333 -p 6334:6334 \
    docker.m.daocloud.io/qdrant/qdrant:v1.12.4

# 4. Temporal
echo "Starting Temporal..."
podman run -d --name temporal --network p0-net -p 7233:7233 \
    temporalio/auto-setup:latest

echo "All infrastructure components started (or starting)."
echo "You can check their status using: podman ps"
