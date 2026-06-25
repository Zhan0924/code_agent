# Load The `llmdoc` Skill First

Before broad source-code exploration, planning, or documentation work, load the `llmdoc` skill.

The main assistant should align with the user before non-trivial plans or edits.

At the end of a non-trivial task, when the work produced durable knowledge, workflow lessons, or useful reflections, the main assistant should proactively use the `llmdoc-update` skill in Codex.

Keep detailed workflow rules, templates, hook behavior, and doc-structure guidance in the `llmdoc` skill.

# Auto-Update Codebase Index

Whenever the assistant modifies code files, it MUST automatically execute the `index_repository` tool from the `codebase-memory-mcp` server to ensure the knowledge graph remains up to date with the latest changes.
