#!/bin/bash
# planning-with-files + audit-driven-fix-loop: Post-tool-use hook for Cursor.
#
# Two responsibilities:
#   1. Always: nudge the agent to update progress.md when task_plan.md exists
#      (original planning-with-files behavior).
#   2. Audit-doc trigger: if the Write/Edit just modified a file matching the
#      audit / defect-doc path patterns (see AGENTS.md "Audit / Defect Doc →
#      Auto-Trigger Fix Loop"), emit a strong reminder telling the agent to
#      start the planning-with-files + audit-driven-fix-loop pipeline.
#
# Cursor passes the tool invocation context as JSON on stdin. We parse it
# best-effort (no jq dependency) to recover the target file path.

set +e

# ─── 1. Original planning-with-files reminder ────────────────────────────
if [ -f task_plan.md ]; then
    echo "[planning-with-files] Update progress.md with what you just did. If a phase is now complete, update task_plan.md status."
fi

# ─── 2. Audit-doc auto-trigger ───────────────────────────────────────────
stdin=""
if [ ! -t 0 ]; then
    stdin=$(cat 2>/dev/null || true)
fi

# Extract a file path from the tool-input JSON. Cursor / various tool
# vendors use different field names (target_file, file_path, path, ...),
# so we try them in order.
target_path=""
if [ -n "$stdin" ]; then
    for field in target_file file_path filePath path targetFile; do
        v=$(printf '%s' "$stdin" \
            | grep -oE "\"$field\"[[:space:]]*:[[:space:]]*\"[^\"]+\"" \
            | head -1 \
            | sed -E "s/.*\"$field\"[[:space:]]*:[[:space:]]*\"([^\"]+)\".*/\1/")
        if [ -n "$v" ]; then
            target_path="$v"
            break
        fi
    done
fi

# Allow env override for manual testing.
if [ -n "${AUDIT_DOC_PATH_OVERRIDE:-}" ]; then
    target_path="$AUDIT_DOC_PATH_OVERRIDE"
fi

[ -z "$target_path" ] && exit 0

# Match audit-doc globs (mirror AGENTS.md "触发路径模式").
is_audit_doc=0
case "$target_path" in
    *-audit.md)
        is_audit_doc=1 ;;
    */doc-gaps.md)
        case "$target_path" in *llmdoc*) is_audit_doc=1 ;; esac ;;
    */docs/superpowers/audits/*.md)
        is_audit_doc=1 ;;
esac

[ "$is_audit_doc" -eq 1 ] || exit 0

# Resolve the absolute path if it's relative to cwd.
abs_path="$target_path"
case "$target_path" in
    /*) abs_path="$target_path" ;;
    *)  abs_path="$(pwd)/$target_path" ;;
esac

# Extract still-open AUDIT-IDs.
# Convention: audit docs declare status via bolded `**AUDIT-...**` in table
# rows. A leading ✅ on the same line marks it as closed. Plain references
# (e.g. "AUDIT-P0-1（反馈闭环）" in prose) are ignored to avoid false alarms.
open_ids=""
if [ -f "$abs_path" ]; then
    # Match any **AUDIT-<id>** style bold tag: AUDIT-, REAUDIT-, MEM-AUDIT-, etc.
    # Convention: a closed ID is preceded by `✅` (with optional whitespace)
    # *immediately* before the bold tag. ✅ appearing elsewhere on the line
    # (e.g. inside another ID's prose description) does NOT close the tag.
    # The pattern `(✅[ \t]*)?**...**` captures any leading ✅; we then
    # filter out matches whose capture starts with ✅, leaving only open IDs.
    open_ids=$(grep -oE "(✅[ 	]*)?\*\*[A-Z]*AUDIT-[A-Z0-9-]+\*\*" "$abs_path" 2>/dev/null \
        | grep -v '^✅' \
        | sed -E 's/\*\*//g' \
        | sort -u \
        | tr '\n' ' ')
fi

cat <<EOF

[audit-doc-trigger] Detected write to audit/defect doc: ${target_path}

You MUST now start the planning-with-files + audit-driven-fix-loop pipeline.
Skipping this is forbidden by AGENTS.md "Audit / Defect Doc → Auto-Trigger
Fix Loop". To skip, you must AskQuestion the user for explicit confirmation.

Required next actions (in order):
  1. Read .claude/skills/audit-driven-fix-loop/SKILL.md (五步循环主索引)
  2. Copy .cursor/skills/planning-with-files/templates/task_plan.md → ./task_plan.md
  3. For every still-open AUDIT-ID below, create one Phase named
     "### Phase N: <AUDIT-ID> — <one-line>" with **Status:** pending
  4. Begin Phase 1 immediately by running audit-driven-fix-loop Step 1
     (code review against actual source, NOT the audit doc's claims)

Still-open AUDIT-IDs in this doc:
  ${open_ids:-<none detected; if the doc is empty/just-written, enumerate IDs after content is added>}

Reminder: each AUDIT-ID must end with Docker deployment + API/log/DB triple
verification before safe-push. One AUDIT-ID per commit. The stop hook will
refuse to let you stop while open AUDIT-IDs remain in this doc.
EOF

exit 0
