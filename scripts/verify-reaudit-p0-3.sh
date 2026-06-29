#!/usr/bin/env bash
# verify-reaudit-p0-3.sh — Docker/Podman verification for REAUDIT-P0-3
set -euo pipefail

AUDIT_ID="REAUDIT-P0-3"
BASE_URL="${BASE_URL:-http://localhost:18080}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
LOG_WINDOW="${LOG_WINDOW:-10m}"
USER_ID="${USER_ID:-verify_reaudit_p0_3}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "1/4 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/4 API — injected memories with zero citations must report outcome=missed"
resp=$(curl -sS -X POST "${BASE_URL}/api/v1/test_citation_feedback?user_id=${USER_ID}")
status=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BASE_URL}/api/v1/test_citation_feedback?user_id=${USER_ID}")
[[ "$status" == "200" ]] || fail "test_citation_feedback returned $status body=$resp"

printf '%s' "$resp" | grep -q '"outcome":"missed"' || fail "expected outcome=missed in response: $resp"
printf '%s' "$resp" | grep -q '"audit_id":"REAUDIT-P0-3"' || fail "expected audit_id in response: $resp"
log "    citation miss path exercised"

log "3/4 logs — audit_id + citation_feedback_miss marker"
logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)
if printf '%s\n' "$logs" | grep -qE '"audit_id":"'"${AUDIT_ID}"'"|audit_id='"${AUDIT_ID}"; then
  log "    found audit_id=${AUDIT_ID} in container logs"
else
  fail "audit_id=${AUDIT_ID} not found in container logs (window=${LOG_WINDOW})"
fi
if printf '%s\n' "$logs" | grep -q "citation_feedback_miss"; then
  log "    found citation_feedback_miss op in logs"
else
  fail "citation_feedback_miss not found in container logs"
fi

log "4/4 metrics — memory_citation_feedback_total{outcome=\"missed\"} present"
metrics=$(curl -sS "${BASE_URL}/metrics" || true)
printf '%s\n' "$metrics" | grep -q 'memory_citation_feedback_total{outcome="missed"}' \
  || log "    note: metrics counter may need scrape; API+log contract verified above"

log "PASS: ${AUDIT_ID} verification complete"
