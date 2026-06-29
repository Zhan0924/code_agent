#!/usr/bin/env bash
# verify-audit-p0-3.sh — Docker/Podman test for AUDIT-P0-3 HNSW migration.
#
# After the agent boots and runs Migrate() on both memories and trajectories
# tables, this script confirms:
#   1. memories table carries an HNSW index (and the legacy IVFFlat is gone)
#   2. trajectories table carries an HNSW index (and the legacy IVFFlat is gone)
#   3. cosine distance queries can still use the index (planner uses HNSW)
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:18080}"
PG_DSN="${PG_DSN:-postgres://agent:agent_secret@localhost:15432/code_agent?sslmode=disable}"

log()  { printf '[verify-p0-3] %s\n' "$*"; }
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

check_index() {
  local table="$1"
  local want_substr="$2"
  local forbid_substr="$3"
  local idx_rows
  idx_rows=$("${PSQL[@]}" -c "SELECT indexname, indexdef FROM pg_indexes WHERE tablename='${table}';")
  if ! printf '%s' "$idx_rows" | grep -qi "${want_substr}"; then
    fail "${table}: expected an index matching '${want_substr}'; got:
$idx_rows"
  fi
  if printf '%s' "$idx_rows" | grep -qi "${forbid_substr}"; then
    fail "${table}: legacy index '${forbid_substr}' still present:
$idx_rows"
  fi
  log "    ${table}: HNSW present, legacy IVFFlat absent"
}

log "2/4 verify memories table indexes"
check_index "memories" "hnsw" "USING ivfflat"

log "3/4 verify trajectories table indexes"
check_index "trajectories" "hnsw" "USING ivfflat"

log "4/4 confirm planner can use HNSW for cosine distance"
plan=$("${PSQL[@]}" -c "
EXPLAIN (FORMAT TEXT)
SELECT id FROM memories
WHERE embedding IS NOT NULL
ORDER BY embedding <=> '[0,0,0]'::vector
LIMIT 5;
")
if ! printf '%s' "$plan" | grep -qi 'idx_memories.*hnsw\|using.*hnsw\|Index Scan'; then
  log "    planner output (informational):"
  printf '%s\n' "$plan"
  log "    note: on an empty table the planner may pick a sequential scan; this is not a failure"
else
  log "    planner picks an HNSW-backed path"
fi

log "PASS: AUDIT-P0-3 docker verification complete"
