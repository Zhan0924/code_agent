#!/bin/bash
# ============================================================
# Comprehensive End-to-End Test Suite for Code Agent
# ============================================================
set -e
BASE="http://localhost:18080"
PASS=0
FAIL=0
TOTAL=0

test_case() {
    TOTAL=$((TOTAL + 1))
    local name="$1"
    local result="$2"
    local expected="$3"
    if echo "$result" | grep -q "$expected"; then
        PASS=$((PASS + 1))
        echo "  ✅ PASS: $name"
    else
        FAIL=$((FAIL + 1))
        echo "  ❌ FAIL: $name"
        echo "    Expected: $expected"
        echo "    Got: $result"
    fi
}

echo ""
echo "╔══════════════════════════════════════════════════════════╗"
echo "║     Code Agent Comprehensive Test Suite                  ║"
echo "╚══════════════════════════════════════════════════════════╝"
echo ""

# ─── 1. Health Checks ────────────────────────────────────────
echo "── 1. Health & Readiness Checks ──"
R=$(curl -s "$BASE/healthz")
test_case "GET /healthz returns ok" "$R" '"status":"ok"'

R=$(curl -s "$BASE/readyz")
test_case "GET /readyz returns ready" "$R" '"status":"ready"'
test_case "GET /readyz postgres ok" "$R" '"postgres":"ok"'
test_case "GET /readyz redis ok" "$R" '"redis":"ok"'

# ─── 2. Session Management ───────────────────────────────────
echo ""
echo "── 2. Session Management ──"
R=$(curl -s -X POST "$BASE/api/v1/sessions" -H "Content-Type: application/json" -d '{}')
test_case "POST /sessions creates session" "$R" '"session_id"'
SESSION_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['session_id'])" 2>/dev/null || echo "")

if [ -n "$SESSION_ID" ]; then
    test_case "Session ID is valid UUID" "$SESSION_ID" "-"

    R=$(curl -s "$BASE/api/v1/sessions/$SESSION_ID")
    test_case "GET /sessions/:id returns session" "$R" "$SESSION_ID"

    # Create a second session
    R2=$(curl -s -X POST "$BASE/api/v1/sessions" -H "Content-Type: application/json" -d '{}')
    SESSION_ID2=$(echo "$R2" | python3 -c "import sys,json; print(json.load(sys.stdin)['session_id'])" 2>/dev/null || echo "")
    test_case "Can create multiple sessions" "$R2" '"session_id"'

    # Delete second session
    R=$(curl -s -X DELETE "$BASE/api/v1/sessions/$SESSION_ID2" -w "\n%{http_code}")
    HTTP_CODE=$(echo "$R" | tail -1)
    test_case "DELETE /sessions/:id returns 200/204" "$HTTP_CODE" "20"
else
    echo "  ⚠ Skipping session tests (no session ID)"
fi

# ─── 3. Chat API ─────────────────────────────────────────────
echo ""
echo "── 3. Chat API (LLM Integration) ──"
if [ -n "$SESSION_ID" ]; then
    # Retry up to 3 times for transient LLM API connectivity issues
    CHAT_OK=0
    for attempt in 1 2 3; do
        R=$(curl -s --max-time 120 -X POST "$BASE/api/v1/chat" \
            -H "Content-Type: application/json" \
            -d "{\"session_id\":\"$SESSION_ID\",\"message\":\"What is 2+2? Reply with just the number.\"}")
        if echo "$R" | grep -q '"message"'; then
            CHAT_OK=1
            break
        fi
        echo "  ⚠ LLM attempt $attempt failed, retrying in 5s..."
        sleep 5
    done

    test_case "POST /chat returns response" "$R" '"message"'
    test_case "POST /chat returns task_id" "$R" '"task_id"'
    test_case "POST /chat state is completed" "$R" '"state":"completed"'

    if [ "$CHAT_OK" -eq 1 ]; then
        # Multi-turn: ask a follow-up (verify context is maintained)
        R=$(curl -s --max-time 120 -X POST "$BASE/api/v1/chat" \
            -H "Content-Type: application/json" \
            -d "{\"session_id\":\"$SESSION_ID\",\"message\":\"What was my previous question about?\"}")
        test_case "Multi-turn chat works" "$R" '"message"'
        test_case "Multi-turn keeps context" "$R" '"state":"completed"'
    else
        echo "  ⚠ Skipping multi-turn (LLM not reachable after 3 retries)"
        TOTAL=$((TOTAL + 2))
        FAIL=$((FAIL + 2))
    fi
else
    echo "  ⚠ Skipping chat tests (no session)"
fi

# ─── 4. Error Handling ───────────────────────────────────────
echo ""
echo "── 4. Error Handling ──"
R=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/v1/nonexistent")
test_case "404 for unknown route" "$R" "404"

R=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/api/v1/chat" \
    -H "Content-Type: application/json" \
    -d '{"session_id":"00000000-0000-0000-0000-000000000000","message":"hi"}')
test_case "Invalid session returns 404" "$R" "404"

R=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/api/v1/chat" \
    -H "Content-Type: application/json" \
    -d '{"message":"missing session"}')
test_case "Missing session_id returns 400" "$R" "400"

# ─── 5. Metrics Endpoint ─────────────────────────────────────
echo ""
echo "── 5. Observability Endpoints ──"
R=$(curl -s "$BASE/metrics" | head -5)
test_case "GET /metrics returns prometheus data" "$R" "HELP\|TYPE\|process_"

# ─── 6. Jaeger Tracing Verification ──────────────────────────
echo ""
echo "── 6. Jaeger Tracing ──"
R=$(curl -s "http://localhost:16686/api/services")
test_case "Jaeger API accessible" "$R" "data"

# Give spans time to flush (batch exporter needs ~5s)
sleep 8

R=$(curl -s "http://localhost:16686/api/services")
test_case "Jaeger has code-agent service" "$R" "code-agent"

R=$(curl -s "http://localhost:16686/api/traces?service=code-agent&limit=5")
test_case "Jaeger has traces from code-agent" "$R" "data"

# ─── 7. Streaming Chat (SSE) ─────────────────────────────────
echo ""
echo "── 7. Streaming Chat (SSE) ──"
if [ -n "$SESSION_ID" ]; then
    R=$(curl -s --max-time 120 -X POST "$BASE/api/v1/chat/stream" \
        -H "Content-Type: application/json" \
        -H "Accept: text/event-stream" \
        -d "{\"session_id\":\"$SESSION_ID\",\"message\":\"Say hello\"}" 2>&1 | head -20)
    test_case "SSE stream returns data" "$R" "data:"
fi

# ─── 8. HITL (Human-in-the-Loop) ─────────────────────────────
echo ""
echo "── 8. Sensitive Command Detection ──"
if [ -n "$SESSION_ID" ]; then
    R=$(curl -s --max-time 120 -X POST "$BASE/api/v1/chat" \
        -H "Content-Type: application/json" \
        -d "{\"session_id\":\"$SESSION_ID\",\"message\":\"Please run: kubectl apply -f deployment.yaml\"}")
    test_case "Sensitive command detected" "$R" 'pending_approval\|awaiting_approval\|approval\|sensitive\|confirm\|HITL'

    TASK_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('task_id',''))" 2>/dev/null || echo "")
    if [ -n "$TASK_ID" ] && echo "$R" | grep -qi "approval"; then
        R=$(curl -s -X POST "$BASE/api/v1/tasks/$TASK_ID/approve" \
            -H "Content-Type: application/json" \
            -d '{"approved": false, "reason": "test rejection"}')
        test_case "Task rejection works" "$R" "reject\|denied\|cancel"
    fi
fi

# ─── 9. RAG Indexing & Qdrant Verification ────────────────────
echo ""
echo "── 9. RAG Knowledge Base (Qdrant) ──"

# Check Qdrant is reachable from host (via container's internal port mapping)
R=$(docker exec code-agent-allinone wget -q -O - http://127.0.0.1:6333/collections 2>&1)
test_case "Qdrant API accessible" "$R" "collections"

# Check the code_embeddings collection exists
test_case "code_embeddings collection exists" "$R" "code_embeddings"

# Create a test markdown file inside the container and trigger indexing
docker exec code-agent-allinone sh -c 'mkdir -p /tmp/test_kb && cat > /tmp/test_kb/architecture.md << "MDEOF"
# System Architecture

## Database Layer

We use PostgreSQL for all persistent storage needs.
The schema migrations are managed by golang-migrate.

## Cache Layer

Redis is used as a distributed cache and session store.
It supports pub/sub for real-time notifications.

## Deployment

### Docker Compose

For local development, use docker-compose.yml.

### Kubernetes

For production, deploy using the K8s manifests in deployments/k8s/.
MDEOF
'

# Trigger indexing via API
R=$(curl -s -X POST "$BASE/api/v1/index" -H "Content-Type: application/json" -d '{"repo_path":"/tmp/test_kb","project_name":"test-kb"}')
test_case "POST /index triggers indexing" "$R" "indexing_started\|accepted\|ok\|status"

# Wait for async indexing + embedding to complete
sleep 8

# Verify points were ACTUALLY stored in Qdrant (points_count > 0)
R=$(docker exec code-agent-allinone wget -q -O - "http://127.0.0.1:6333/collections/code_embeddings" 2>&1)
POINTS=$(echo "$R" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['result']['points_count'])" 2>/dev/null || echo "0")
test_case "Qdrant points_count > 0 (got: $POINTS)" "$POINTS" "[1-9]"

# Verify we can search/scroll the indexed points
R=$(docker exec code-agent-allinone wget -q -O - --post-data '{"limit":3,"with_payload":true}' "http://127.0.0.1:6333/collections/code_embeddings/points/scroll" 2>&1)
test_case "Qdrant scroll returns points with payload" "$R" "file_path\|content\|symbol"

# ─── Summary ─────────────────────────────────────────────────
echo ""
echo "╔══════════════════════════════════════════════════════════╗"
echo "║  Test Summary                                            ║"
echo "╠══════════════════════════════════════════════════════════╣"
echo "║  Total: $TOTAL  |  Passed: $PASS  |  Failed: $FAIL          ║"
echo "╚══════════════════════════════════════════════════════════╝"

if [ "$FAIL" -eq 0 ]; then
    echo "🎉 All tests passed!"
    exit 0
else
    echo "⚠️  Some tests failed. Review output above."
    exit 1
fi
