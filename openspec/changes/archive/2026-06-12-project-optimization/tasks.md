## 1. Setup and Centralize Constants

- [x] 1.1 Create `internal/models/tool_names.go` defining all built-in and common tool constants.
- [x] 1.2 Update `internal/orchestrator/tool_names.go` to use the constants defined in `internal/models` to maintain compatibility.

## 2. Refactor Packages to Use Centralized Constants

- [x] 2.1 Refactor `internal/multiagent` package to replace hardcoded tool string literals with `models` constants.
- [x] 2.2 Refactor `internal/agentloop` package to replace hardcoded tool string literals with `models` constants.
- [x] 2.3 Refactor `internal/planner` package to replace hardcoded tool string literals with `models` constants.
- [x] 2.4 Refactor `internal/tools` package to replace hardcoded tool string literals with `models` constants.

## 3. Verify Changes

- [x] 3.1 Run all unit tests to ensure no regressions were introduced.
