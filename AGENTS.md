# Load The `llmdoc` Skill First

Before broad source-code exploration, planning, or documentation work, load the `llmdoc` skill.

The main assistant should align with the user before non-trivial plans or edits.

At the end of a non-trivial task, when the work produced durable knowledge, workflow lessons, or useful reflections, the main assistant should proactively use the `llmdoc-update` skill in Codex.

Keep detailed workflow rules, templates, hook behavior, and doc-structure guidance in the `llmdoc` skill.

# Auto-Update Codebase Index

Whenever the assistant modifies code files, it MUST automatically execute the `index_repository` tool from the `codebase-memory-mcp` server to ensure the knowledge graph remains up to date with the latest changes. 
IMPORTANT: You must pass the `repo_path` parameter in `Arguments` (e.g., `{"repo_path": "/Users/zhanqiankun.1/code/code_agent"}`). Do not call this tool without the required `repo_path` parameter.

# Use `writing-plans` Skill

Whenever the assistant is tasked with generating technical plans or outlining implementation steps, it MUST explicitly load and read the `writing-plans` skill before beginning to draft the plan.

# Strict MCP Usage Guardrail

**ZERO-TOLERANCE RULE**: The assistant is STRICTLY FORBIDDEN from using `grep_search` or `list_dir` for finding code definitions, structs, or logic without FIRST calling `codebase-memory-mcp` tools (`search_graph`, `trace_path`, or `query_graph`). The assistant MUST explicitly state in its `<thought>` block why the MCP tool is insufficient before falling back to grep.
IMPORTANT: All querying tools (like `search_graph`, `query_graph`, `trace_path`) require the `project` parameter. This parameter MUST be the project name returned by `list_projects` (e.g., `"Users-zhanqiankun.1-code-code_agent"`), NOT the absolute path or folder name. If you encounter a `project not found or not indexed` error, immediately run `list_projects` to get the correct project name.

