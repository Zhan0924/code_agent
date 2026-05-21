#!/bin/bash
# ============================================================
# test_integration.sh - HTTP-level integration tests inside Docker
# ============================================================
# Runs the api package's integration_test.go which spins up a full
# httptest.Server + miniredis + zap observer and exercises real HTTP
# endpoints, asserting on both status codes and captured structured logs.
#
# Output is saved to /tmp/code_agent_integration_output.log for analysis.
# ============================================================
set -euo pipefail

DOCKER_IMAGE="docker.1ms.run/golang:1.24-alpine"
PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_FILE="/tmp/code_agent_integration_output.log"

echo "============================================================"
echo "  Code Agent — HTTP Integration Test Runner"
echo "  Image:   $DOCKER_IMAGE"
echo "  Project: $PROJECT_DIR"
echo "  Output:  $OUTPUT_FILE"
echo "============================================================"
echo ""

# Pre-flight: make sure miniredis is in go.sum. If not, go mod tidy inside container.
docker run --rm \
    -v "$PROJECT_DIR":/app \
    -w /app \
    -e GOTOOLCHAIN=auto \
    -e GOPROXY=https://goproxy.cn,direct \
    -e GOFLAGS=-mod=mod \
    -e GIN_MODE=release \
    "$DOCKER_IMAGE" \
    sh -c '
set -e
apk add --no-cache git >/dev/null 2>&1
git config --global user.email "ci@code-agent.local"
git config --global user.name "CI Test Runner"

echo "=== Step 0: Ensure miniredis dependency ==="
go get github.com/alicebob/miniredis/v2@latest
go mod tidy
echo "DEPS: OK"
echo ""

echo "=== Step 1: Compile integration_test.go ==="
go test -c -o /tmp/api_integration.test ./internal/api/
echo "COMPILE: PASS"
echo ""

echo "=== Step 2: Run HTTP Integration Tests (verbose) ==="
echo "------------------------------------------------------------"
# -v: verbose, show each t.Log output (our request traces & log dumps)
# -count=1: disable cache so we always run
# -run: filter to only the Integration tests
go test -v -count=1 -timeout 120s \
    -run "TestIntegration_" \
    ./internal/api/ 2>&1 | tee /tmp/integration_output.log

echo ""
echo "=== Step 3: Summary ==="
PASS_COUNT=$(grep -c "^--- PASS:" /tmp/integration_output.log || echo 0)
FAIL_COUNT=$(grep -c "^--- FAIL:" /tmp/integration_output.log || echo 0)
SKIP_COUNT=$(grep -c "^--- SKIP:" /tmp/integration_output.log || echo 0)
echo "PASS=$PASS_COUNT  FAIL=$FAIL_COUNT  SKIP=$SKIP_COUNT"

if [ -f /tmp/code_agent_test_artifacts/integration_trace.txt ]; then
  echo ""
  echo "=== Integration Trace Artifact ==="
  cat /tmp/code_agent_test_artifacts/integration_trace.txt
fi

# Copy log out of container via volume mount by placing in /app
cp /tmp/integration_output.log /app/integration_output.log
if [ -f /tmp/code_agent_test_artifacts/integration_trace.txt ]; then
  cp /tmp/code_agent_test_artifacts/integration_trace.txt /app/integration_trace.txt
fi

if [ "$FAIL_COUNT" -gt 0 ]; then
  exit 1
fi
'

# Surface artifact to the host
if [ -f "$PROJECT_DIR/integration_output.log" ]; then
  cp "$PROJECT_DIR/integration_output.log" "$OUTPUT_FILE"
  echo ""
  echo "============================================================"
  echo "  Full log copied to: $OUTPUT_FILE"
  echo "  Trace (if present): $PROJECT_DIR/integration_trace.txt"
  echo "============================================================"
fi
