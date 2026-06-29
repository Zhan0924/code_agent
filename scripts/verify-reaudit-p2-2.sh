#!/usr/bin/env bash
# verify-reaudit-p2-2.sh — integration test verification for REAUDIT-P2-2
set -euo pipefail

AUDIT_ID="REAUDIT-P2-2"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

log()  { printf '[verify-%s] %s\n' "$AUDIT_ID" "$*"; }
fail() { log "FAIL: $*"; exit 1; }

log "1/2 list split integration tests"
tests=$(grep -E '^func TestPGCold_Integration_' "${REPO_ROOT}/internal/memory/pg_cold_integration_test.go" | wc -l | tr -d ' ')
[[ "$tests" -ge 4 ]] || fail "expected >=4 split TestPGCold_Integration_* functions, got $tests"
log "    found $tests integration test functions"

log "2/2 run integration tests (requires podman/docker for testcontainers)"
cd "${REPO_ROOT}"
export TESTCONTAINERS_RYUK_DISABLED="${TESTCONTAINERS_RYUK_DISABLED:-true}"
if [[ -z "${DOCKER_HOST:-}" ]]; then
  for sock in \
    "unix://${XDG_RUNTIME_DIR}/podman/podman.sock" \
    "unix://${HOME}/.local/share/containers/podman/machine/podman.sock" \
    "unix:///var/run/docker.sock"; do
    if [[ -S "${sock#unix://}" ]]; then
      export DOCKER_HOST="$sock"
      break
    fi
  done
fi
if go test ./internal/memory/ -run 'TestPGCold_Integration_' -count=1 -timeout=10m -v 2>&1 | tee /tmp/verify-p2-2.log | tail -20; then
  log "PASS: ${AUDIT_ID} integration tests green"
else
  fail "integration tests failed — see /tmp/verify-p2-2.log"
fi
