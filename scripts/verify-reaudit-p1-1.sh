#!/usr/bin/env bash
# verify-reaudit-p1-1.sh — Docker/Podman verification for REAUDIT-P1-1
set -euo pipefail

AUDIT_ID="REAUDIT-P1-1"
BASE_URL="${BASE_URL:-http://localhost:18080}"
CONTAINER_NAME="${CONTAINER_NAME:-code-agent}"
LOG_WINDOW="${LOG_WINDOW:-10m}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MAX_CORE_LINES="${MAX_CORE_LINES:-120}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "1/4 structure — hybrid.go core-only + split files present"
core_lines=$(wc -l < "${REPO_ROOT}/internal/memory/hybrid.go" | tr -d ' ')
[[ "$core_lines" -le "$MAX_CORE_LINES" ]] || fail "hybrid.go has ${core_lines} lines (max ${MAX_CORE_LINES})"

for f in hybrid_embed hybrid_store hybrid_retrieve hybrid_list hybrid_admin hybrid_queues hybrid_decay hybrid_dedup hybrid_rrf; do
  [[ -f "${REPO_ROOT}/internal/memory/${f}.go" ]] || fail "missing internal/memory/${f}.go"
done
log "    hybrid.go=${core_lines} lines; 9 domain files present"

log "2/4 unit test — TestHybridStore_FileSplit_REAUDIT_P1_1"
cd "${REPO_ROOT}"
go test ./internal/memory/... -run TestHybridStore_FileSplit_REAUDIT_P1_1 -count=1 -short \
  || fail "structure contract test failed"

log "3/4 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "4/4 API — memory explain path still wired (smoke)"
# Use a random id; 404 is fine — we only need routing + handler alive.
status=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/api/v1/memory/explain/nonexistent-verify-p1-1")
[[ "$status" == "404" || "$status" == "200" ]] || fail "memory explain returned $status"

logs=$(podman logs --since "$LOG_WINDOW" "$CONTAINER_NAME" 2>&1 || true)
if printf '%s\n' "$logs" | grep -q "component.*memory.hybrid"; then
  log "    agent booted with memory.hybrid component"
else
  log "    note: boot log outside window; healthz+tests passed"
fi

log "PASS: ${AUDIT_ID} verification complete"
