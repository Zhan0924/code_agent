## Context

Currently, tool name strings (e.g. `"read_file"`, `"write_file"`) are hardcoded in multiple packages, specifically `internal/multiagent`, `internal/agentloop`, and `internal/planner`. Although the `internal/orchestrator` package has centralized its own internal usage in `tool_names.go`, cross-package references still use hardcoded literals because orchestrator cannot be imported by other packages without introducing import cycles.

## Goals / Non-Goals

**Goals:**
- Centralize all built-in and common tool name constants in `internal/models/tool_names.go`.
- Replace all hardcoded tool name string literals in `multiagent`, `agentloop`, `planner`, and `tools` with references to the centralized constants.
- Avoid introducing any circular dependency/import cycle.
- Keep the design backward-compatible for existing `orchestrator` files by defining aliases in `internal/orchestrator/tool_names.go`.

**Non-Goals:**
- Completely restructuring how tools are registered or executed.
- Modifying the tool schemas or execution interfaces.

## Decisions

### Decision 1: Shared Constants Location
We will define the constants in the leaf package `internal/models` (specifically `internal/models/tool_names.go`).
- *Rationale*: `internal/models` is a leaf package that does not import any internal package except standard libraries. It can be safely imported by any package (`orchestrator`, `multiagent`, `agentloop`, `planner`, `tools`) without causing import cycles.
- *Alternatives Considered*:
  - Defining them in `internal/tools`: This causes cycles because `tools` package might be imported by packages that also define tools, or circular dependencies with packages that need to resolve tools.
  - Creating a new package `internal/tools/names`: Too heavyweight for a single file of constants.

### Decision 2: Orchestrator Compatibility via Aliases
Instead of refactoring all files inside the `orchestrator` package, `internal/orchestrator/tool_names.go` will be kept but modified to define constants mapped to the `models` constants (e.g., `const ToolReadFile = models.ToolReadFile`).
- *Rationale*: Minimizes unnecessary churn in the `orchestrator` package while ensuring a single source of truth.

## Risks / Trade-offs

- **Risk**: A typo during string replacement could break a tool lookup.
  - *Mitigation*: Run all automated tests (`make test`) and run the compiler to ensure all references resolve correctly.
