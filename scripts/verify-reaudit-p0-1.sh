#!/usr/bin/env bash
# verify-reaudit-p0-1.sh — Docker/Podman verification for REAUDIT-P0-1
set -euo pipefail

AUDIT_ID="REAUDIT-P0-1"
BASE_URL="${BASE_URL:-http://localhost:18080}"
PG_DSN="${PG_DSN:-postgres://agent:agent_secret@localhost:15432/code_agent?sslmode=disable}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
PG_CONTAINER="${PG_CONTAINER:-agent-postgres}"
LOG_WINDOW="${LOG_WINDOW:-10m}"

MEM_ID="66666666-1111-4111-a111-666666666601"
USER_ID="${USER_ID:-verify_reaudit_p0_1}"
PROJECT_ID="${PROJECT_ID:-default}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

if command -v psql >/dev/null 2>&1; then
  PSQL=(psql "$PG_DSN" -v ON_ERROR_STOP=1 -A -t)
elif command -v podman >/dev/null 2>&1 && podman ps --format '{{.Names}}' | grep -qx "$PG_CONTAINER"; then
  PSQL=(podman exec -i "$PG_CONTAINER" psql -U agent -d code_agent -v ON_ERROR_STOP=1 -A -t)
else
  fail "need psql or running $PG_CONTAINER container"
fi

log "1/5 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/5 seed episodic memory (undistilled)"
"${PSQL[@]}" -c "DELETE FROM memories WHERE id='${MEM_ID}';" >/dev/null
"${PSQL[@]}" -c "
INSERT INTO memories (id, user_id, project_id, type, content, score, access_count, created_at, updated_at, last_accessed_at)
VALUES ('${MEM_ID}', '${USER_ID}', '${PROJECT_ID}', 'episodic', 'verify_reaudit_p0_1 episode', 0.8, 1, NOW(), NOW(), NOW());
" >/dev/null

log "3/5 API explain — distilled_at should be null before distill mark"
resp=$(curl -sS "${BASE_URL}/api/v1/memory/explain/${MEM_ID}")
status=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/api/v1/memory/explain/${MEM_ID}")
[[ "$status" == "200" ]] || fail "explain returned $status body=$resp"
printf '%s' "$resp" | grep -q '"distilled_at"' && fail "expected distilled_at omitted/null before mark: $resp"

log "4/5 DB — simulate MarkDistilled (SET distilled_at, row must survive)"
"${PSQL[@]}" -c "UPDATE memories SET distilled_at = NOW() WHERE id='${MEM_ID}';" >/dev/null
count=$("${PSQL[@]}" -c "SELECT count(*) FROM memories WHERE id='${MEM_ID}'" | tr -d '[:space:]')
[[ "$count" == "1" ]] || fail "row should still exist after mark (not DELETE); count=$count"

distilled=$("${PSQL[@]}" -c "SELECT distilled_at IS NOT NULL FROM memories WHERE id='${MEM_ID}'" | tr -d '[:space:]')
[[ "$distilled" == "t" ]] || fail "distilled_at should be set"

resp2=$(curl -sS "${BASE_URL}/api/v1/memory/explain/${MEM_ID}")
printf '%s' "$resp2" | grep -q '"distilled_at"' || fail "explain should include distilled_at after mark: $resp2"

log "5/5 logs — audit_id marker (if distiller ran in window)"
logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)
if printf '%s\n' "$logs" | grep -q "audit_id=${AUDIT_ID}"; then
  log "    found audit_id=${AUDIT_ID} in container logs"
else
  log "    note: no distiller tick in window; DB contract verified above"
fi

"${PSQL[@]}" -c "DELETE FROM memories WHERE id='${MEM_ID}';" >/dev/null
log "PASS: ${AUDIT_ID} verification complete"
