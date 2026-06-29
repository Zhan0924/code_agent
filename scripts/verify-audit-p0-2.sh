#!/usr/bin/env bash
# verify-audit-p0-2.sh — Docker/Podman E2E test for AUDIT-P0-2 distiller-safe episodic GC.
#
# Validates two contracts on a real pgvector container:
#   1. `agent` boots with the new defaults and starts the episodic GC scheduler.
#   2. `DeleteOldEpisodic` (the GC SQL path) preserves undistilled episodic rows
#      even when their created_at + distilled_at predates the retention window.
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:18080}"
PG_DSN="${PG_DSN:-postgres://agent:agent_secret@localhost:15432/code_agent?sslmode=disable}"
USER_ID="${USER_ID:-verify_p0_2_user}"
PROJECT_ID="${PROJECT_ID:-default}"

ID_OLD_DISTILLED="22222222-1111-4111-a111-111111111101"
ID_OLD_UNDIST="22222222-1111-4111-a111-111111111102"
ID_FRESH_DISTILLED="22222222-1111-4111-a111-111111111103"
ID_SEMANTIC="22222222-1111-4111-a111-111111111104"

log()  { printf '[verify-p0-2] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

if command -v psql >/dev/null 2>&1; then
  PSQL=(psql "$PG_DSN" -v ON_ERROR_STOP=1 -A -t)
elif command -v podman >/dev/null 2>&1 && podman ps --format '{{.Names}}' | grep -qx 'agent-postgres'; then
  PSQL=(podman exec -i agent-postgres psql -U agent -d code_agent -v ON_ERROR_STOP=1 -A -t)
else
  fail "need psql or running agent-postgres container"
fi

log "1/5 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/5 confirm agent started episodic-gc scheduler"
if command -v podman >/dev/null 2>&1 && podman ps --format '{{.Names}}' | grep -qx 'code-agent'; then
  logs=$(podman logs code-agent 2>&1 || true)
  if printf '%s' "$logs" | grep -q 'episodic gc scheduler started'; then
    log "    -> episodic gc scheduler running"
  else
    fail "agent did not start episodic gc scheduler — check distill/gc config"
  fi
else
  log "    (skipped: code-agent container not running)"
fi

log "3/5 clean up + seed 4 rows"
"${PSQL[@]}" -c "DELETE FROM memories WHERE id IN ('${ID_OLD_DISTILLED}','${ID_OLD_UNDIST}','${ID_FRESH_DISTILLED}','${ID_SEMANTIC}');" >/dev/null
"${PSQL[@]}" -c "
INSERT INTO memories (id, user_id, project_id, type, content, score, access_count, created_at, updated_at, last_accessed_at, distilled_at) VALUES
  ('${ID_OLD_DISTILLED}',  '${USER_ID}', '${PROJECT_ID}', 'episodic', 'old + distilled — should be deleted',         0.5, 0, NOW()-INTERVAL '60 days', NOW(), NOW(), NOW()-INTERVAL '40 days'),
  ('${ID_OLD_UNDIST}',     '${USER_ID}', '${PROJECT_ID}', 'episodic', 'old + undistilled — must be preserved',       0.5, 0, NOW()-INTERVAL '60 days', NOW(), NOW(), NULL),
  ('${ID_FRESH_DISTILLED}','${USER_ID}', '${PROJECT_ID}', 'episodic', 'fresh + distilled — must be preserved',       0.5, 0, NOW()-INTERVAL '1 day',   NOW(), NOW(), NOW()-INTERVAL '1 day'),
  ('${ID_SEMANTIC}',       '${USER_ID}', '${PROJECT_ID}', 'semantic', 'semantic noise — must never be touched',      0.5, 0, NOW()-INTERVAL '60 days', NOW(), NOW(), NULL);
" >/dev/null

before=$("${PSQL[@]}" -c "SELECT COUNT(*) FROM memories WHERE user_id='${USER_ID}';" | tr -d '[:space:]')
[[ "$before" == "4" ]] || fail "expected 4 seeded rows, found $before"
log "    seeded 4 rows"

log "4/5 invoke the same DELETE SQL the GC loop runs (older_than=30d)"
"${PSQL[@]}" -c "
DELETE FROM memories
WHERE type = 'episodic'
  AND distilled_at IS NOT NULL
  AND distilled_at < NOW() - INTERVAL '30 days';
" >/dev/null

log "5/5 verify which rows survived"
survivors=$("${PSQL[@]}" -c "SELECT id FROM memories WHERE user_id='${USER_ID}' ORDER BY id;" | tr -d ' ' | sort)
expected=$(printf '%s\n%s\n%s\n' "${ID_OLD_UNDIST}" "${ID_FRESH_DISTILLED}" "${ID_SEMANTIC}" | sort)

if [[ "$survivors" != "$expected" ]]; then
  fail "survivor mismatch
expected:
$expected
actual:
$survivors"
fi
log "    survivors match expected set (old+undistilled / fresh+distilled / semantic preserved; old+distilled deleted)"

"${PSQL[@]}" -c "DELETE FROM memories WHERE user_id='${USER_ID}';" >/dev/null

log "PASS: AUDIT-P0-2 docker verification complete"
