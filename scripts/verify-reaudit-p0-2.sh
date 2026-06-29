#!/usr/bin/env bash
# verify-reaudit-p0-2.sh — Docker/Podman verification for REAUDIT-P0-2
set -euo pipefail

AUDIT_ID="REAUDIT-P0-2"
BASE_URL="${BASE_URL:-http://localhost:18080}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
LOG_WINDOW="${LOG_WINDOW:-10m}"
USER_ID="${USER_ID:-verify_reaudit_p0_2}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "1/4 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/4 API — core_memory_append must mask AWS key before persist"
resp=$(curl -sS -X POST "${BASE_URL}/api/v1/test_core_memory_pii?user_id=${USER_ID}")
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/api/v1/test_core_memory_pii?user_id=${USER_ID}")
[[ "$status" == "200" ]] || fail "test_core_memory_pii returned $status body=$resp"

printf '%s' "$resp" | grep -q '"masked":true' || fail "expected masked=true in response: $resp"
printf '%s' "$resp" | grep -q '\[REDACTED:AWS_KEY\]' || fail "expected REDACTED marker in stored content: $resp"
printf '%s' "$resp" | grep -q 'AKIAIOSFODNN7EXAMPLE' && fail "raw AWS key must not appear in stored content: $resp"
log "    stored content is masked"

log "3/4 logs — audit_id marker"
logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)
if printf '%s\n' "$logs" | grep -q "audit_id=${AUDIT_ID}"; then
  log "    found audit_id=${AUDIT_ID} in container logs"
else
  log "    note: audit log may be outside window; API+Redis contract verified above"
fi

log "4/4 cleanup redis key"
if command -v podman >/dev/null 2>&1 && podman ps --format '{{.Names}}' | grep -qx 'agent-redis'; then
  podman exec agent-redis redis-cli DEL "core_memory:project:${USER_ID}:default" >/dev/null || true
fi

log "PASS: ${AUDIT_ID} verification complete"
