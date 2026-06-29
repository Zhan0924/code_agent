#!/usr/bin/env bash
# verify-reaudit-p1-3.sh — Docker/Podman verification for REAUDIT-P1-3
set -euo pipefail

AUDIT_ID="REAUDIT-P1-3"
BASE_URL="${BASE_URL:-http://localhost:18080}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
LOG_WINDOW="${LOG_WINDOW:-10m}"
USER_ID="${USER_ID:-verify_reaudit_p1_3}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "1/4 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/4 API — duplicate core_memory_append must dedup"
resp=$(curl -sS -X POST "${BASE_URL}/api/v1/test_core_memory_dedup?user_id=${USER_ID}")
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/api/v1/test_core_memory_dedup?user_id=${USER_ID}")
[[ "$status" == "200" ]] || fail "test_core_memory_dedup returned $status body=$resp"
printf '%s' "$resp" | grep -q '"deduped":true' || fail "expected deduped=true: $resp"
printf '%s' "$resp" | grep -q '"line_hits":1' || fail "expected line_hits=1: $resp"
log "    core memory dedup OK"

log "3/4 logs — audit_id marker"
logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)
if printf '%s\n' "$logs" | grep -qE '"audit_id":"'"${AUDIT_ID}"'"|audit_id='"${AUDIT_ID}"; then
  log "    found audit_id=${AUDIT_ID} in container logs"
else
  fail "audit_id=${AUDIT_ID} not found in container logs"
fi

log "4/4 cleanup redis key"
if command -v podman >/dev/null 2>&1 && podman ps --format '{{.Names}}' | grep -qx 'agent-redis'; then
  podman exec agent-redis redis-cli DEL "core_memory:project:${USER_ID}:default" >/dev/null || true
fi

log "PASS: ${AUDIT_ID} verification complete"
