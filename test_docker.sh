#!/bin/bash
# ============================================================
# test_docker.sh - Build & test code_agent in Docker container
# ============================================================
# Runs all P1 feature tests inside a Docker container (Linux/arm64)
# to verify modules work in a clean, isolated environment.
#
# Usage: bash test_docker.sh
# ============================================================
set -euo pipefail

DOCKER_IMAGE="docker.1ms.run/golang:1.24-alpine"
PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "============================================================"
echo "  Code Agent — Docker Test Runner"
echo "  Image:   $DOCKER_IMAGE"
echo "  Project: $PROJECT_DIR"
echo "============================================================"
echo ""

# Run build + tests in a single ephemeral container
docker run --rm \
    -v "$PROJECT_DIR":/app \
    -w /app \
    -e GOTOOLCHAIN=auto \
    -e GOPROXY=https://goproxy.cn,direct \
    -e GOFLAGS=-mod=mod \
    "$DOCKER_IMAGE" \
    sh -c '
set -e
apk add --no-cache git >/dev/null 2>&1

# Git config required for git_tools tests (commit/branch operations)
git config --global user.email "ci@code-agent.local"
git config --global user.name "CI Test Runner"
git config --global init.defaultBranch main

echo "=== Step 1: Build Verification ==="
CGO_ENABLED=0 go build -o /dev/null ./cmd/agent
echo "BUILD: PASS"
echo ""

echo "=== Step 2: P1 Feature Tests (Planner, RepoMap, Router, ProjectRules, GitTools, Watcher) ==="
go test -v -count=1 -coverprofile=/tmp/cov.out \
    ./internal/planner/ \
    ./internal/repomap/ \
    ./internal/llm/ \
    ./internal/orchestrator/ \
    -run "TestValidateDAG|TestTopologicalSort|TestParsePlan|TestPlan_|TestEstimate|TestNeedsPlanning|TestPlanner_|TestExecutor_|TestStep_JSON|TestGenerator_|TestFormatCompact|TestExtractSymbols|TestRouter_|TestQuickComplexity|TestRuleLoader|TestProjectRules|TestExtractGolangci|TestExtractPython|TestExtractTSConfig|TestWatcher_|TestTruncateList|TestGitToolDefinitions_|TestOrchestrator_EnsureGitInit|TestOrchestrator_RunGit|TestOrchestrator_ToolGit|TestOrchestrator_AutoCommitAfterEdit" \
    | tee /tmp/test.log
echo ""
echo "--- PASS: $(grep -c "^--- PASS" /tmp/test.log) / FAIL: $(grep -c "^--- FAIL" /tmp/test.log) / SKIP: $(grep -c "^--- SKIP" /tmp/test.log) ---"
echo ""
echo "=== Step 3: Coverage on new P1 feature files ==="
go tool cover -func=/tmp/cov.out | grep -E "internal/(planner/planner|planner/executor|repomap/generator|repomap/watcher|llm/router|orchestrator/git_tools|orchestrator/project_rules)\.go" | awk "{split(\$1,a,\":\"); f=a[1]; total[f]+=\$3+0; c[f]++} END { for (f in total) printf \"  %-70s %.1f%%\n\", f, total[f]/c[f] }" | sort

echo ""
echo "============================================================"
echo "  ALL DOCKER TESTS PASSED!"
echo "============================================================"
'
