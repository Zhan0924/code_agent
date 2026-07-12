#!/bin/bash
# planning-with-files + audit-driven-fix-loop: Stop hook for Cursor.
#
# Two-layer gate before the agent is allowed to stop:
#
#   Layer 1 (planning-with-files, original):
#     If task_plan.md exists, refuse stop while any Phase is not "complete".
#
#   Layer 2 (audit-driven-fix-loop, added):
#     If any audit/defect doc under the configured globs still contains
#     AUDIT-IDs without a ✅ mark, refuse stop with a followup_message that
#     spells out the remaining IDs.
#
# Always exits 0 — flow control is via JSON stdout (followup_message) and
# hooks.json `loop_limit` (capped to prevent infinite re-prompts).

set +e

PLAN_FILE="task_plan.md"

# ─── Layer 1: planning-with-files phase gate ─────────────────────────────
plan_layer_msg=""
plan_block=0
if [ -f "$PLAN_FILE" ]; then
    TOTAL=$(grep -c "### Phase" "$PLAN_FILE" || true)
    COMPLETE=$(grep -cF "**Status:** complete" "$PLAN_FILE" || true)
    IN_PROGRESS=$(grep -cF "**Status:** in_progress" "$PLAN_FILE" || true)
    PENDING=$(grep -cF "**Status:** pending" "$PLAN_FILE" || true)

    if [ "${COMPLETE:-0}" -eq 0 ] && [ "${IN_PROGRESS:-0}" -eq 0 ] && [ "${PENDING:-0}" -eq 0 ]; then
        COMPLETE=$(grep -c "\[complete\]" "$PLAN_FILE" || true)
        IN_PROGRESS=$(grep -c "\[in_progress\]" "$PLAN_FILE" || true)
        PENDING=$(grep -c "\[pending\]" "$PLAN_FILE" || true)
    fi

    : "${TOTAL:=0}"
    : "${COMPLETE:=0}"
    : "${IN_PROGRESS:=0}"
    : "${PENDING:=0}"

    if [ "$TOTAL" -gt 0 ] && [ "$COMPLETE" -lt "$TOTAL" ]; then
        plan_block=1
        plan_layer_msg="[planning-with-files] Task incomplete (${COMPLETE}/${TOTAL} phases done). Update progress.md, then read task_plan.md and continue working on the remaining phases."
    fi
fi

# ─── Layer 2: audit-driven-fix-loop closure gate ────────────────────────
audit_layer_msg=""
audit_block=0
audit_docs=""

# Only run Layer 2 inside repos that actually host audit docs, to avoid
# noise in unrelated projects. Activation signals:
#   - llmdoc/ directory exists, OR
#   - docs/superpowers/audits/ directory exists
if [ -d "llmdoc" ] || [ -d "docs/superpowers/audits" ]; then
    # Collect candidate audit docs (mirror AGENTS.md path patterns).
    audit_docs=$( {
        find llmdoc -type f -name '*-audit.md' 2>/dev/null
        find llmdoc -type f -name 'doc-gaps.md' 2>/dev/null
        find docs -type f -name '*-audit.md' 2>/dev/null
        find docs/superpowers/audits -type f -name '*.md' 2>/dev/null
    } | sort -u )

    open_summary=""
    if [ -n "$audit_docs" ]; then
        while IFS= read -r doc; do
            [ -z "$doc" ] && continue
            # Open IDs = bolded `**AUDIT-...**` (status declaration in a
            # table row) on a line without ✅. Plain prose references like
            # "AUDIT-P0-1（反馈闭环）" are intentionally ignored.
            # Match any **AUDIT-<id>** style bold tag: AUDIT-, REAUDIT-, MEM-AUDIT-, etc.
            # Convention: a closed ID is preceded by `✅` (with optional
            # whitespace) *immediately* before the bold tag. ✅ appearing
            # elsewhere on the line (e.g. inside another ID's prose) does
            # NOT close the tag. Capture optional `✅` prefix, then drop
            # any match starting with ✅.
            open=$(grep -oE "(✅[ 	]*)?\*\*[A-Z]*AUDIT-[A-Z0-9-]+\*\*" "$doc" 2>/dev/null \
                | grep -v '^✅' \
                | sed -E 's/\*\*//g' \
                | sort -u \
                | tr '\n' ' ')
            if [ -n "$open" ]; then
                open_summary="${open_summary}    - ${doc}: ${open}\\n"
            fi
        done <<EOF
$audit_docs
EOF
    fi

    if [ -n "$open_summary" ]; then
        audit_block=1
        audit_layer_msg="[audit-driven-fix-loop] Refusing to stop: still-open AUDIT-IDs detected.\\n${open_summary}Next: ensure task_plan.md has one Phase per AUDIT-ID, then run audit-driven-fix-loop Step 1-5 for each. See .claude/skills/audit-driven-fix-loop/SKILL.md."
    fi
fi

# ─── Compose response ────────────────────────────────────────────────────
if [ "$plan_block" -eq 1 ] || [ "$audit_block" -eq 1 ]; then
    combined=""
    [ "$plan_block" -eq 1 ] && combined="${plan_layer_msg}"
    if [ "$audit_block" -eq 1 ]; then
        if [ -n "$combined" ]; then
            combined="${combined}\\n\\n${audit_layer_msg}"
        else
            combined="${audit_layer_msg}"
        fi
    fi
    printf '{"followup_message": "%s"}\n' "$combined"
    exit 0
fi

# All-clear path: optionally emit completion summary if a plan existed.
if [ -f "$PLAN_FILE" ] && [ "${TOTAL:-0}" -gt 0 ]; then
    printf '{"followup_message": "[planning-with-files] ALL PHASES COMPLETE (%s/%s). If the user has additional work, add new phases to task_plan.md before starting."}\n' "$COMPLETE" "$TOTAL"
fi
exit 0
