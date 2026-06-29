#!/usr/bin/env bash
# verify-reaudit-p1-2.sh — Docker/Podman verification for REAUDIT-P1-2
set -euo pipefail

AUDIT_ID="REAUDIT-P1-2"
BASE_URL="${BASE_URL:-http://localhost:18080}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
LOG_WINDOW="${LOG_WINDOW:-10m}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "1/4 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/4 API — memory list without user_id uses anonymous/default"
stats=$(curl -sS "${BASE_URL}/api/v1/memory/stats")
printf '%s' "$stats" | grep -q '"user_id":"anonymous"' || fail "stats missing anonymous user_id: $stats"
printf '%s' "$stats" | grep -q '"project_id":"default"' || fail "stats missing default project_id: $stats"
log "    memory/stats tenant fallback OK"

log "3/4 API — orchestrator tenant normalize endpoint"
resp=$(curl -sS -X POST "${BASE_URL}/api/v1/test_tenant_normalize")
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/api/v1/test_tenant_normalize")
[[ "$status" == "200" ]] || fail "test_tenant_normalize returned $status body=$resp"
printf '%s' "$resp" | grep -q '"user_id":"anonymous"' || fail "expected anonymous in response: $resp"
printf '%s' "$resp" | grep -q '"project_id":"default"' || fail "expected default project in response: $resp"
log "    orchestrator resolveTenantIDs OK"

log "4/4 logs — tenant_normalize marker"
logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)
if printf '%s\n' "$logs" | grep -qE '"audit_id":"'"${AUDIT_ID}"'"|audit_id='"${AUDIT_ID}"; then
  log "    found audit_id=${AUDIT_ID} in container logs"
else
  fail "audit_id=${AUDIT_ID} not found in container logs"
fi

log "PASS: ${AUDIT_ID} verification complete"
