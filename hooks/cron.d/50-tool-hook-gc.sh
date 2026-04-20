#!/usr/bin/env bash
# 50-tool-hook-gc.sh — Garbage-collect the tool-hook audit table.
#
# Runs in the post-hook phase of cron. The audit table grows unbounded
# (every Claude Code hook event across all sessions writes a row); without
# cleanup it hits millions of rows in a few weeks and slows queries.
#
# Default retention: 30 days. Override by setting WEIRAN_TOOL_HOOK_GC_DAYS.
#
# Only runs during cron mode, not heartbeat — heartbeat fires hourly and
# gc should not run that often.
#
# Env: WEIRAN_MODE, WEIRAN_WORKSPACE, WEIRAN_DB

set -euo pipefail

# Skip outside cron mode.
if [[ "${WEIRAN_MODE:-}" != "cron" ]]; then
    exit 0
fi

DAYS="${WEIRAN_TOOL_HOOK_GC_DAYS:-30}"
WEIRAN_BIN="${WEIRAN_BIN:-$HOME/.local/bin/weiran}"

if [[ ! -x "$WEIRAN_BIN" ]]; then
    echo "[tool-hook-gc] $WEIRAN_BIN not found or not executable — skipping" >&2
    exit 0
fi

# Run gc; captures "deleted N rows older than D days" line to stdout so it
# surfaces in the cron report Telegram summary.
OUT=$("$WEIRAN_BIN" tool-hook gc --days "$DAYS" 2>&1 || true)
echo "[tool-hook-gc] $OUT"
