#!/usr/bin/env bash
# verify-reaudit-p1-4.sh — Docker/Podman verification for REAUDIT-P1-4
set -euo pipefail

AUDIT_ID="REAUDIT-P1-4"
BASE_URL="${BASE_URL:-http://localhost:18080}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
LOG_WINDOW="${LOG_WINDOW:-10m}"
USER_ID="${USER_ID:-verify_reaudit_p1_4}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "1/4 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/4 API — structured cited_memory_ids must resolve without regex"
resp=$(curl -sS -X POST "${BASE_URL}/api/v1/test_feedback_cited_resolve?user_id=${USER_ID}")
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/api/v1/test_feedback_cited_resolve?user_id=${USER_ID}")
[[ "$status" == "200" ]] || fail "test_feedback_cited_resolve returned $status body=$resp"
printf '%s' "$resp" | grep -q '"cited_source":"structured"' || fail "expected cited_source=structured: $resp"
printf '%s' "$resp" | grep -q '"resolved":true' || fail "expected resolved=true: $resp"
log "    structured citation resolve OK"

log "3/4 logs — audit_id marker"
logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)
if printf '%s\n' "$logs" | grep -qE '"audit_id":"'"${AUDIT_ID}"'"|audit_id='"${AUDIT_ID}"; then
  log "    found audit_id=${AUDIT_ID} in container logs"
else
  fail "audit_id=${AUDIT_ID} not found in container logs"
fi

log "4/4 PASS marker"
log "PASS: ${AUDIT_ID} verification complete"
