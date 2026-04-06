#!/usr/bin/env bash
# 90-safety-extended.sh — Extended safety checks (supplements Go built-in checks)
# Runs in the post-hook phase of cron/heartbeat
# Environment variables: WEIRAN_MODE, WEIRAN_WORKSPACE, WEIRAN_DB

set -euo pipefail

WS="${WEIRAN_WORKSPACE:-$HOME/.openclaw/workspace}"
WARNINGS=()

# ── 1. Memory pollution: daily notes should not reference non-existent files ──
today=$(date +%Y-%m-%d)
daily="$WS/memory/$today.md"
if [[ -f "$daily" ]]; then
    while IFS= read -r path; do
        resolved="$WS/$path"
        if [[ ! -e "$resolved" && ! -e "$HOME/$path" ]]; then
            WARNINGS+=("daily notes references non-existent path: $path")
        fi
    done < <(grep -oE 'projects/[a-zA-Z0-9_-]+/[a-zA-Z0-9_./-]+' "$daily" 2>/dev/null | head -20 || true)
fi

# ── 2. Git health: should not have massive uncommitted changes ──
if cd "$WS" 2>/dev/null; then
    untracked=$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')
    if [[ "$untracked" -gt 50 ]]; then
        WARNINGS+=("too many uncommitted files: ${untracked} (may need cleanup or commit)")
    fi

    while IFS=$'\t' read -r adds dels file; do
        if [[ -n "$adds" && "$adds" != "-" ]] && [[ "$adds" -gt 500 ]] 2>/dev/null; then
            WARNINGS+=("large file diff: $file (+${adds} lines)")
        fi
    done < <(git diff --numstat 2>/dev/null | head -30 || true)
fi

# ── 3. Temp file accumulation ──
tmp_count=$(find /tmp -maxdepth 1 -name "weiran-*" -mmin +120 2>/dev/null | wc -l | tr -d ' ')
if [[ "$tmp_count" -gt 5 ]]; then
    WARNINGS+=("${tmp_count} stale weiran temp files in /tmp (older than 2 hours)")
fi

# ── 4. openclaw.json integrity (must be valid JSON) ──
oc_json="$HOME/.openclaw/openclaw.json"
if [[ -f "$oc_json" ]]; then
    if ! jq empty "$oc_json" 2>/dev/null; then
        WARNINGS+=("openclaw.json is not valid JSON!")
    fi
fi

# ── Output ──
if [[ ${#WARNINGS[@]} -eq 0 ]]; then
    echo "[safety-extended] all checks passed" >&2
    exit 0
fi

echo "[safety-extended] found ${#WARNINGS[@]} issue(s):" >&2
for w in "${WARNINGS[@]}"; do
    echo "  $w" >&2
done

# No Telegram notification here (Go layer handles alerting)
exit 0
