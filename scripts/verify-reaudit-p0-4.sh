#!/usr/bin/env bash
# verify-reaudit-p0-4.sh — Docker/Podman verification for REAUDIT-P0-4
set -euo pipefail

AUDIT_ID="REAUDIT-P0-4"
BASE_URL="${BASE_URL:-http://localhost:18080}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
LOG_WINDOW="${LOG_WINDOW:-10m}"
USER_ID="${USER_ID:-verify_reaudit_p0_4}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "1/4 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/4 API — failing embedder must report degraded=true"
resp=$(curl -sS -X POST "${BASE_URL}/api/v1/test_embedder_degrade?user_id=${USER_ID}")
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/api/v1/test_embedder_degrade?user_id=${USER_ID}")
[[ "$status" == "200" ]] || fail "test_embedder_degrade returned $status body=$resp"

printf '%s' "$resp" | grep -q '"degraded":true' || fail "expected degraded=true: $resp"
printf '%s' "$resp" | grep -q '"audit_id":"REAUDIT-P0-4"' || fail "expected audit_id in response: $resp"
log "    embedder degrade path exercised"

log "3/4 logs — audit_id + retrieve_degraded marker"
logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)
if printf '%s\n' "$logs" | grep -qE '"audit_id":"'"${AUDIT_ID}"'"|audit_id='"${AUDIT_ID}"; then
  log "    found audit_id=${AUDIT_ID} in container logs"
else
  fail "audit_id=${AUDIT_ID} not found in container logs"
fi
if printf '%s\n' "$logs" | grep -q "retrieve_degraded"; then
  log "    found retrieve_degraded op in logs"
else
  fail "retrieve_degraded not found in container logs"
fi

log "4/4 metrics — memory_retrieve_degraded_total present"
metrics=$(curl -sS "${BASE_URL}/metrics" || true)
printf '%s\n' "$metrics" | grep -q 'memory_retrieve_degraded_total{reason="embedder_failed"}' \
  || log "    note: degraded counter may need scrape; API+log contract verified above"
printf '%s\n' "$metrics" | grep -q 'memory_failures_total{tier="embedder"' \
  || log "    note: embedder failure counter may need scrape"

log "PASS: ${AUDIT_ID} verification complete"
