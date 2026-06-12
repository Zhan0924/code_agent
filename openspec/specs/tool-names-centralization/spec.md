# tool-names-centralization Specification

## Purpose
TBD - created by archiving change project-optimization. Update Purpose after archive.
## Requirements
### Requirement: Centralized Tool Name Constants
The system SHALL define a single centralized source of truth for all built-in and common tool name constants inside the leaf package `internal/models`, ensuring that any package can access these constants without introducing import cycles.

#### Scenario: Centralized Constants Definition
- **WHEN** a developer inspects `internal/models/tool_names.go`
- **THEN** it defines constants for all standard tool names including `read_file`, `write_file`, `patch_file`, `edit_file`, `apply_diff`, `list_files`, `create_directory`, `run_tests`, `run_workspace_cmd`, `shell_exec`, `execute_code`, `git_status`, `git_diff`, `git_commit`, `git_log`, `git_branch`, `rename_symbol`, `search_code`, `goto_definition`, `find_references`, `hover_info`, and `document_symbols`

#### Scenario: Core Orchestrator References Centralized Constants
- **WHEN** the orchestrator registers built-in tools or routes tool execution
- **THEN** it references the centralized tool name constants in `internal/models` instead of hardcoded strings or package-local constants

#### Scenario: Multiagent and Planner References Centralized Constants
- **WHEN** the multiagent supervisor, sub-agent pool, role selector, or planner filters, schedules, or rates tool usage
- **THEN** they reference the centralized constants in `internal/models` instead of hardcoded strings

