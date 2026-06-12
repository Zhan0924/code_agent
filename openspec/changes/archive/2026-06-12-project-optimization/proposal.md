## Why

Currently, tool name strings (like `"read_file"`, `"write_file"`, `"execute_code"`, etc.) are duplicated as hardcoded string literals across multiple packages (`multiagent`, `agentloop`, `planner`, `orchestrator`, `tools`). This makes adding, removing, or renaming tools extremely fragile and error-prone, as it requires codebase-wide search-and-replace, and risks introducing mismatches. Centralizing them resolves these maintenance and reliability issues.

## What Changes

- Create a shared `tool_names.go` file within the leaf package `internal/models` to define all built-in and common tool name constants.
- Migrate the local constants in `internal/orchestrator/tool_names.go` to the centralized `internal/models/tool_names.go`.
- Replace all hardcoded tool name string literals across the `multiagent`, `agentloop`, `planner`, `orchestrator`, and `tools` packages with references to the new centralized constants.
- Ensure that the refactoring is backward-compatible and does not introduce import cycles.

## Capabilities

### New Capabilities
- `tool-names-centralization`: Centralize cross-package tool name constants to resolve tool name duplication and fragmentation.

### Modified Capabilities
<!-- None -->

## Impact

This refactoring affects:
- `internal/models`: Defines the centralized tool name constants.
- `internal/orchestrator`: Relies on centralized constants instead of local ones.
- `internal/multiagent`: Replaces hardcoded string literals with centralized constants.
- `internal/agentloop`: Replaces hardcoded string literals with centralized constants.
- `internal/planner`: Replaces hardcoded string literals with centralized constants.
- `internal/tools`: Replaces hardcoded string literals with centralized constants.
- Unit and integration tests in these packages.
