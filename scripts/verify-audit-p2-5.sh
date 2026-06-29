#!/usr/bin/env bash
# verify-audit-p2-5.sh — Docker/Podman test for AUDIT-P2-5 memory
# explainability:
#   1. GET /api/v1/memory/explain/:id returns 200 + full row for a seeded memory.
#   2. GET /api/v1/memory/explain/:id returns 404 for an unknown id.
#   3. Audit log "memories injected into prompt" contract is testable via
#      structured fields (validated by unit test, not exercised here).
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:18080}"
PG_DSN="${PG_DSN:-postgres://agent:agent_secret@localhost:15432/code_agent?sslmode=disable}"
USER_ID="${USER_ID:-verify_p2_5_user}"
PROJECT_ID="${PROJECT_ID:-default}"
MEM_ID="55555555-1111-4111-a111-555555555501"

log()  { printf '[verify-p2-5] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

if command -v psql >/dev/null 2>&1; then
  PSQL=(psql "$PG_DSN" -v ON_ERROR_STOP=1 -A -t)
elif command -v podman >/dev/null 2>&1 && podman ps --format '{{.Names}}' | grep -qx 'agent-postgres'; then
  PSQL=(podman exec -i agent-postgres psql -U agent -d code_agent -v ON_ERROR_STOP=1 -A -t)
else
  fail "need psql or running agent-postgres container"
fi

log "1/4 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/4 seed an explainable memory"
"${PSQL[@]}" -c "DELETE FROM memories WHERE id='${MEM_ID}';" >/dev/null
"${PSQL[@]}" -c "
INSERT INTO memories (id, user_id, project_id, type, content, score, access_count, created_at, updated_at, last_accessed_at)
VALUES ('${MEM_ID}', '${USER_ID}', '${PROJECT_ID}', 'preference', 'verify_p2_5 prefers tabs', 0.91, 7, NOW(), NOW(), NOW());
" >/dev/null
log "    seeded id=${MEM_ID}"

log "3/4 GET /api/v1/memory/explain/<known-id>"
resp=$(curl -sS "${BASE_URL}/api/v1/memory/explain/${MEM_ID}")
status=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/api/v1/memory/explain/${MEM_ID}")
[[ "$status" == "200" ]] || fail "expected HTTP 200 for known id; got ${status}, body=${resp}"

for field in '"id"' '"user_id"' '"project_id"' '"type"' '"content"' '"score"' '"access_count"' '"created_at"'; do
  printf '%s' "$resp" | grep -q "${field}" || fail "explain response missing field ${field}: ${resp}"
done
printf '%s' "$resp" | grep -q '"verify_p2_5 prefers tabs"' || fail "explain response content mismatch: ${resp}"
log "    explain returned full row; content matches"

log "4/4 GET /api/v1/memory/explain/<unknown-id> returns 404"
status=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/api/v1/memory/explain/does-not-exist")
[[ "$status" == "404" ]] || fail "expected HTTP 404 for unknown id; got ${status}"
log "    unknown id correctly returns 404"

"${PSQL[@]}" -c "DELETE FROM memories WHERE id='${MEM_ID}';" >/dev/null

log "PASS: AUDIT-P2-5 docker verification complete"
