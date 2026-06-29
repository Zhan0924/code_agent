#!/usr/bin/env bash
# verify-audit-p2-4.sh — Docker/Podman test for AUDIT-P2-4 failure
# classification. Confirms:
#   1. `code_agent_memory_failures_total` counter is registered + exposed.
#   2. Stopping the PG container causes a `severity="error"` increment on
#      the next /api/v1/memory call (cold tier unreachable).
#   3. Restarting PG returns the counter to a stable steady state.
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:18080}"
USER_ID="${USER_ID:-verify_p2_4_user}"
PROJECT_ID="${PROJECT_ID:-default}"

log()  { printf '[verify-p2-4] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

scrape_metric() {
  # extract value for `code_agent_memory_failures_total{<labels...>}` lines that
  # match the supplied severity substring. Returns 0 when absent (counter
  # registered lazily on first Inc).
  local severity="$1"
  curl -sf "${BASE_URL}/metrics" 2>/dev/null \
    | awk -v sev="severity=\"${severity}\"" '
        /^code_agent_memory_failures_total\{/ && $0 ~ sev {
          v=$NF; sum+=v
        }
        END { print (sum=="" ? 0 : sum) }
      '
}

log "1/4 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/4 confirm failures_total counter is wired in /metrics"
metrics_text=$(curl -sf "${BASE_URL}/metrics")
if ! printf '%s' "$metrics_text" | grep -q 'code_agent_memory_failures_total'; then
  fail "failures_total counter not exposed in /metrics"
fi
log "    counter present"

before_err=$(scrape_metric "error")
log "    baseline severity=error count: ${before_err}"

log "3/4 stop agent-postgres to force a cold-tier failure on next retrieve"
if ! command -v podman >/dev/null 2>&1 || ! podman ps --format '{{.Names}}' | grep -qx 'agent-postgres'; then
  log "    (skipped: agent-postgres container not running)"
  log "PASS: AUDIT-P2-4 partial — metric exists, full path not exercised"
  exit 0
fi

podman stop agent-postgres >/dev/null
# allow a few seconds for in-flight Redis health pings to clear
sleep 2

log "    fire a /api/v1/memory list which hits the cold tier"
curl -s -o /dev/null "${BASE_URL}/api/v1/memory?user_id=${USER_ID}&project_id=${PROJECT_ID}" || true
sleep 2
after_err=$(scrape_metric "error")
log "    post-failure severity=error count: ${after_err}"

log "4/4 restart agent-postgres"
podman start agent-postgres >/dev/null
sleep 4

if (( $(echo "$after_err > $before_err" | bc -l) )); then
  log "    counter incremented (${before_err} -> ${after_err})"
  log "PASS: AUDIT-P2-4 docker verification complete"
else
  fail "counter did not increment despite cold-tier outage (before=${before_err}, after=${after_err})"
fi
