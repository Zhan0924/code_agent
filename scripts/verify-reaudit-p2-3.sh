#!/usr/bin/env bash
# verify-reaudit-p2-3.sh — repo hygiene verification for REAUDIT-P2-3
set -euo pipefail

AUDIT_ID="REAUDIT-P2-3"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE_URL="${BASE_URL:-http://localhost:18080}"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

removed=(
  split_hybrid.py
  list_funcs.py
  proxy_forwarder.py
  pull_aggressively.sh
  test_feedback.go
  test_pii.go
  .test_session_id
  docs/superpowers/plans/2026-06-29-AUDIT-P2-1-hybrid-refactor.md
  docs/superpowers/plans/2026-06-29-episodic-gc.md
  docs/superpowers/plans/2026-06-29-gdpr-delete.md
  docs/superpowers/plans/2026-06-29-pg-integration-test.md
)

log "1/2 removed paths must not exist"
for f in "${removed[@]}"; do
  [[ ! -e "${REPO_ROOT}/${f}" ]] || fail "still exists: ${f}"
done
log "    all ${#removed[@]} hygiene targets absent"

log "2/2 agent healthz still OK"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "PASS: ${AUDIT_ID} verification complete"
