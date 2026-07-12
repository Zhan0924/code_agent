#!/usr/bin/env bash
# verify-<AUDIT-ID>.sh — Docker/Podman three-tier verification for <AUDIT-ID>
#
# Skeleton from: .claude/skills/audit-driven-fix-loop/templates/verify-template.sh
# Usage:
#   cp .claude/skills/audit-driven-fix-loop/templates/verify-template.sh \
#       scripts/verify-<AUDIT-ID>.sh
#   chmod +x scripts/verify-<AUDIT-ID>.sh
#   ./scripts/verify-<AUDIT-ID>.sh
#
# Three tiers (all required):
#   1. API   — curl assertions on /api/v1/<endpoint>
#   2. Logs  — podman logs grep on structured `audit_id=<AUDIT-ID>` lines
#   3. DB    — psql assertions on persisted state
#
# Replace every <PLACEHOLDER> before running.

set -euo pipefail

# ─── Variables (edit per AUDIT-ID) ───────────────────────────────────────
AUDIT_ID="<AUDIT-ID>"
BASE_URL="${BASE_URL:-http://localhost:18080}"
PG_DSN="${PG_DSN:-postgres://agent:agent_secret@localhost:15432/code_agent?sslmode=disable}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
PG_CONTAINER="${PG_CONTAINER:-agent-postgres}"
LOG_WINDOW="${LOG_WINDOW:-5m}"

USER_ID="${USER_ID:-verify_${AUDIT_ID,,}_user}"
PROJECT_ID="${PROJECT_ID:-default}"

# ─── Helpers ─────────────────────────────────────────────────────────────
log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }
pass() { log "PASS: $*"; }

# ─── 0. Health check ─────────────────────────────────────────────────────
log "0/5 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

# ─── Detect psql ─────────────────────────────────────────────────────────
if command -v psql >/dev/null 2>&1; then
  PSQL=(psql "$PG_DSN" -v ON_ERROR_STOP=1 -A -t)
elif command -v podman >/dev/null 2>&1 && \
     podman ps --format '{{.Names}}' | grep -qx "$PG_CONTAINER"; then
  PSQL=(podman exec -i "$PG_CONTAINER" psql -U agent -d code_agent -v ON_ERROR_STOP=1 -A -t)
else
  fail "need psql or running $PG_CONTAINER container"
fi

# ─── 1. Seed DB state (optional, comment out if not needed) ──────────────
log "1/5 seed precondition data"
"${PSQL[@]}" -c "
-- Replace with the rows your scenario needs.
-- Example:
-- DELETE FROM memories WHERE user_id='${USER_ID}';
-- INSERT INTO memories (id, user_id, project_id, type, content, score, created_at, updated_at, last_accessed_at)
-- VALUES ('aaaaaaaa-1111-4111-a111-aaaaaaaa0001', '${USER_ID}', '${PROJECT_ID}', 'preference', 'seed for ${AUDIT_ID}', 0.9, NOW(), NOW(), NOW());
SELECT 1;
" >/dev/null

# ─── 2. API verification ─────────────────────────────────────────────────
log "2/5 API checks"

# 2a) Positive: happy path
endpoint="/api/v1/<your-endpoint>"
resp=$(curl -sS "${BASE_URL}${endpoint}")
status=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}${endpoint}")
[[ "$status" == "200" ]] || fail "expected 200 for ${endpoint}; got ${status}, body=${resp}"

for field in '"<field_a>"' '"<field_b>"'; do
  printf '%s' "$resp" | grep -q "$field" \
    || fail "missing expected field $field in response: $resp"
done
pass "API positive: ${endpoint} returns 200 with required fields"

# 2b) Negative: error path
neg_endpoint="/api/v1/<your-endpoint>/does-not-exist"
neg_status=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}${neg_endpoint}")
[[ "$neg_status" == "404" ]] \
  || fail "expected 404 for ${neg_endpoint}; got ${neg_status}"
pass "API negative: unknown id returns 404"

# ─── 3. Log verification ─────────────────────────────────────────────────
log "3/5 log assertions (window=${LOG_WINDOW})"
logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)

# 3a) Required line present
printf '%s\n' "$logs" | grep -q "audit_id=${AUDIT_ID}" \
  || fail "expected audit_id=${AUDIT_ID} log line missing"

# 3b) Result == ok somewhere
printf '%s\n' "$logs" \
  | grep "audit_id=${AUDIT_ID}" \
  | grep -q '"result":"ok"' \
  || fail "expected result=ok line for ${AUDIT_ID} not found"

# 3c) No critical severity in window
if printf '%s\n' "$logs" | grep "audit_id=${AUDIT_ID}" | grep -q '"severity":"critical"'; then
  fail "unexpected critical severity log emitted during verification"
fi
pass "log assertions passed"

# ─── 4. DB verification ──────────────────────────────────────────────────
log "4/5 DB assertions"

# Example: row count assertion
count=$("${PSQL[@]}" -c "SELECT count(*) FROM memories WHERE user_id='${USER_ID}'" | tr -d '[:space:]')
# Replace expected value & predicate as needed
expected="<expected_count>"
if [[ "$expected" != "<expected_count>" ]]; then
  [[ "$count" == "$expected" ]] \
    || fail "expected ${expected} rows for ${USER_ID}; got ${count}"
  pass "DB row count check: ${count} == ${expected}"
fi

# Example: field value assertion
# val=$("${PSQL[@]}" -c "SELECT score FROM memories WHERE id='<id>'" | tr -d '[:space:]')
# [[ "$val" == "<expected>" ]] || fail "expected score=<expected>, got $val"

# ─── 5. Cleanup seed data ────────────────────────────────────────────────
log "5/5 cleanup seed"
"${PSQL[@]}" -c "DELETE FROM memories WHERE user_id='${USER_ID}';" >/dev/null

log "PASS: ${AUDIT_ID} docker verification complete (API + logs + DB)"
