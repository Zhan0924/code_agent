#!/usr/bin/env bash
# verify-reaudit-p2-1.sh — Docker/Podman verification for REAUDIT-P2-1
set -euo pipefail

AUDIT_ID="REAUDIT-P2-1"
BASE_URL="${BASE_URL:-http://localhost:18080}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
LOG_WINDOW="${LOG_WINDOW:-10m}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "1/3 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/3 logs — dedup_oversample in effective config"
logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)
printf '%s\n' "$logs" | grep -q '"dedup_oversample":' || fail "dedup_oversample missing from startup logs"
log "    dedup_oversample present in effective config"

log "3/3 logs — audit_id marker"
if printf '%s\n' "$logs" | grep -qE '"audit_id":"'"${AUDIT_ID}"'"|audit_id='"${AUDIT_ID}"; then
  log "    found audit_id=${AUDIT_ID} in container logs"
else
  fail "audit_id=${AUDIT_ID} not found in container logs"
fi

log "PASS: ${AUDIT_ID} verification complete"
