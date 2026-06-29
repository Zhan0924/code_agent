#!/usr/bin/env bash
# verify-audit-p0-1.sh — Docker/Podman E2E smoke test for AUDIT-P0-1 citation feedback loop.
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:18080}"
API="${BASE_URL}/api/v1"
PG_DSN="${PG_DSN:-postgres://agent:agent_secret@localhost:15432/code_agent?sslmode=disable}"
REDIS_ADDR="${REDIS_ADDR:-localhost:16379}"
MEM_ID="${MEM_ID:-11111111-1111-4111-a111-111111111111}"
USER_ID="${USER_ID:-verify_user}"
PROJECT_ID="${PROJECT_ID:-default}"
MSG_ID="${MSG_ID:-msg-audit-p0-1-verify}"

TMPDIR="${TMPDIR:-/tmp}/verify-p0-1-$$"
mkdir -p "$TMPDIR"
export TMPDIR
trap 'rm -rf "$TMPDIR"' EXIT

log() { printf '[verify-p0-1] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

need_cmd curl
need_cmd python3

if command -v psql >/dev/null 2>&1; then
  PSQL=(psql "$PG_DSN" -v ON_ERROR_STOP=1)
elif command -v podman >/dev/null 2>&1 && podman ps --format '{{.Names}}' | grep -qx 'agent-postgres'; then
  PSQL=(podman exec -i agent-postgres psql -U agent -d code_agent -v ON_ERROR_STOP=1)
else
  fail "need psql or running agent-postgres container"
fi

redis_cli() {
  if command -v redis-cli >/dev/null 2>&1; then
    redis-cli -h "${REDIS_ADDR%%:*}" -p "${REDIS_ADDR##*:}" "$@"
  elif command -v podman >/dev/null 2>&1 && podman ps --format '{{.Names}}' | grep -qx 'agent-redis'; then
    podman exec -i agent-redis redis-cli "$@"
  else
    fail "need redis-cli or running agent-redis container"
  fi
}

log "1/8 healthz"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
[[ "$code" == "200" ]] || fail "healthz returned $code"

log "2/8 seed cold memory in postgres (id=$MEM_ID score=0.50)"
"${PSQL[@]}" -c "
INSERT INTO memories (id, user_id, project_id, type, content, score, access_count, created_at, updated_at, last_accessed_at)
VALUES ('${MEM_ID}', '${USER_ID}', '${PROJECT_ID}', 'preference', 'verify user prefers tabs', 0.50, 0, NOW(), NOW(), NOW())
ON CONFLICT (id) DO UPDATE SET score = 0.50, content = EXCLUDED.content, user_id = EXCLUDED.user_id, project_id = EXCLUDED.project_id;
"

log "3/8 seed hot memory in redis"
NOW_ISO=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
export MEM_ID USER_ID PROJECT_ID NOW_ISO
python3 - <<'PY' >"$TMPDIR/mem.json"
import json, os
print(json.dumps({
    "id": os.environ["MEM_ID"],
    "user_id": os.environ["USER_ID"],
    "project_id": os.environ["PROJECT_ID"],
    "type": "preference",
    "content": "verify user prefers tabs",
    "score": 0.50,
    "access_count": 0,
    "created_at": os.environ["NOW_ISO"],
    "updated_at": os.environ["NOW_ISO"],
    "last_accessed_at": os.environ["NOW_ISO"],
}))
PY
redis_cli SET "memory:${USER_ID}:${PROJECT_ID}:${MEM_ID}" "$(cat "$TMPDIR/mem.json")" EX 86400 >/dev/null

log "4/8 create session"
curl -sf -X POST "${API}/sessions" \
  -H 'Content-Type: application/json' \
  -d "{\"user_id\":\"${USER_ID}\",\"project_id\":\"${PROJECT_ID}\"}" >"$TMPDIR/session.json"
SESSION_ID=$(python3 -c 'import json; print(json.load(open("'"$TMPDIR/session.json"'"))["session_id"])')
[[ -n "$SESSION_ID" ]] || fail "empty session_id"
log "session_id=$SESSION_ID"

log "5/8 inject assistant message with duplicate [mem:id] citations into redis session"
redis_cli GET "sess:hot:${SESSION_ID}" >"$TMPDIR/sess_raw.json"
export MSG_ID MEM_ID NOW_ISO USER_ID PROJECT_ID
python3 - <<'PY' >"$TMPDIR/sess_updated.json"
import json, os
path = os.environ["TMPDIR"] + "/sess_raw.json"
with open(path) as f:
    raw = f.read().strip()
if not raw:
    raise SystemExit("session hot key missing")
sess = json.loads(raw)
msg = {
    "id": os.environ["MSG_ID"],
    "role": "assistant",
    "content": f"Answer citing [mem:{os.environ['MEM_ID']}] twice [mem:{os.environ['MEM_ID']}]",
    "timestamp": os.environ["NOW_ISO"],
    "token_count": 12,
}
sess.setdefault("messages", []).append(msg)
sess["updated_at"] = os.environ["NOW_ISO"]
print(json.dumps(sess))
PY
redis_cli -x SET "sess:hot:${SESSION_ID}" <"$TMPDIR/sess_updated.json" >/dev/null
redis_cli EXPIRE "sess:hot:${SESSION_ID}" 3600 >/dev/null

log "6/8 GET /memory before feedback"
curl -sf "${API}/memory?user_id=${USER_ID}&project_id=${PROJECT_ID}&limit=50" >"$TMPDIR/before.json"
export MEM_ID
python3 - <<'PY'
import json, os, sys
mem_id = os.environ["MEM_ID"]
data = json.load(open(os.environ["TMPDIR"] + "/before.json"))
score = None
for m in data.get("memories", []):
    if m.get("id") == mem_id:
        score = float(m.get("score", 0))
        break
if score is None:
    sys.exit("seed memory not visible via GET /memory")
if abs(score - 0.50) > 1e-6:
    sys.exit(f"expected score 0.50 before feedback, got {score}")
print("before_score_ok", score)
PY

log "7/8 POST feedback (+1)"
HTTP_CODE=$(curl -s -o "$TMPDIR/feedback.json" -w '%{http_code}' -X POST \
  "${API}/sessions/${SESSION_ID}/messages/${MSG_ID}/feedback" \
  -H 'Content-Type: application/json' \
  -d '{"score":1}')
[[ "$HTTP_CODE" == "200" ]] || fail "feedback HTTP $HTTP_CODE body=$(cat "$TMPDIR/feedback.json")"
python3 - <<'PY'
import json, os, sys
body = json.load(open(os.environ["TMPDIR"] + "/feedback.json"))
if body.get("memories_affected") != 1:
    sys.exit(f"expected memories_affected=1, got {body}")
print("memories_affected_ok", body["memories_affected"])
PY

log "8/8 GET /memory after feedback — expect score 0.60"
curl -sf "${API}/memory?user_id=${USER_ID}&project_id=${PROJECT_ID}&limit=50" >"$TMPDIR/after.json"
python3 - <<'PY'
import json, os, sys
mem_id = os.environ["MEM_ID"]
data = json.load(open(os.environ["TMPDIR"] + "/after.json"))
score = None
for m in data.get("memories", []):
    if m.get("id") == mem_id:
        score = float(m.get("score", 0))
        break
if score is None:
    sys.exit("memory missing after feedback")
if abs(score - 0.60) > 1e-6:
    sys.exit(f"expected 0.60 after +0.1 boost, got {score}")
print("after_score_ok", score)
PY

if command -v podman >/dev/null 2>&1 && podman ps --format '{{.Names}}' | grep -qx 'code-agent'; then
  log "agent logs (recent boost-related):"
  podman logs code-agent --since 10m 2>&1 | grep -E 'boosted cited memories|failed to boost' || log "(no auto-boost log lines — expected unless LLM chat cited a memory)"
fi

log "PASS: AUDIT-P0-1 docker verification complete"
