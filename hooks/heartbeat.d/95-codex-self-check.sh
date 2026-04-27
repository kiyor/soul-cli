#!/usr/bin/env bash
#
# 95-codex-self-check.sh — heartbeat-time codex backend liveness probe.
#
# Runs the direct codex app-server handshake (stage 1 of codex-smoke.sh)
# on a daily cadence so we notice if the codex binary breaks (auth
# expired, OAuth refresh failed, codex crashed) without waiting for a
# user-driven session to fail.
#
# Cadence: skip unless `date +%H` matches CODEX_SELF_CHECK_HOUR (default
# 04 = 4am local). The heartbeat runs every 30min so this would fire
# twice if we didn't gate; the second run is also short-circuited by a
# state file under $WEIRAN_DB/.. so we only ever fire once per day.
#
# Failures send a Telegram notification via `weiran notify` if available.
# We never fail the heartbeat itself (exit 0 always) so a transient codex
# outage doesn't cascade into a heartbeat-failure alert.

set -u  # do NOT set -e: best-effort, never block the heartbeat

CODEX_BIN="${CODEX_BIN:-/opt/homebrew/bin/codex}"
SELF_CHECK_HOUR="${CODEX_SELF_CHECK_HOUR:-04}"

# Hour gate
HOUR=$(date +%H)
if [[ "$HOUR" != "$SELF_CHECK_HOUR" ]]; then
  exit 0
fi

# Once-per-day gate via state file
STATE_DIR="${WEIRAN_DB:-$HOME/.openclaw/data}"
mkdir -p "$STATE_DIR" 2>/dev/null || true
TODAY=$(date +%Y-%m-%d)
MARKER="$STATE_DIR/.codex-self-check-$TODAY"
if [[ -f "$MARKER" ]]; then
  exit 0
fi
touch "$MARKER" 2>/dev/null || true

# Find smoke script (live next to this hook in the source tree, or
# installed alongside the weiran binary)
SCRIPT_DIR="${WEIRAN_WORKSPACE:-$HOME/.openclaw/workspace}/scripts/weiran/scripts"
SMOKE="$SCRIPT_DIR/codex-smoke.sh"
if [[ ! -x "$SMOKE" ]]; then
  # Best-effort fallback: search common dev paths
  for c in \
      "$HOME/.openclaw/workspace/scripts/weiran/scripts/codex-smoke.sh" \
      "$HOME/code/weiran/scripts/codex-smoke.sh"; do
    if [[ -x "$c" ]]; then
      SMOKE="$c"
      break
    fi
  done
fi

if [[ ! -x "$SMOKE" ]]; then
  echo "[codex-self-check] smoke script not found, skipping" >&2
  exit 0
fi

# Run only stage 1 (direct probe), never stage 2 — stage 2 needs the
# weiran server we're hosting and would self-loop.
LOG=$(mktemp -t codex-self-check.XXXXXX.log)
if env -u WEIRAN_SERVER_URL -u WEIRAN_AUTH_TOKEN \
    CODEX_BIN="$CODEX_BIN" \
    CODEX_PROMPT="self-check from weiran heartbeat: reply ok" \
    "$SMOKE" >"$LOG" 2>&1; then
  echo "[codex-self-check] OK on $TODAY" >&2
  rm -f "$LOG"
  exit 0
fi

# Failure path
TAIL=$(tail -20 "$LOG" 2>/dev/null || true)
echo "[codex-self-check] FAILED on $TODAY" >&2
echo "$TAIL" >&2

if command -v weiran >/dev/null 2>&1; then
  MSG=$(printf "[codex-self-check] FAILED %s\nTail:\n%s" "$TODAY" "$TAIL")
  weiran notify "$MSG" >/dev/null 2>&1 || true
fi

rm -f "$LOG"
exit 0  # never block heartbeat
