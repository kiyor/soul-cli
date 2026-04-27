#!/usr/bin/env bash
#
# codex-smoke.sh — codex backend end-to-end smoke test (Round 5)
#
# Two stages:
#   1. Direct probe — fork `codex app-server --listen stdio://` and complete
#      initialize → initialized → thread/start → turn/start → turn/completed
#      ourselves over JSON-RPC 2.0. Validates the protocol works on this box
#      independent of weiran.
#   2. Server probe — POST /api/sessions backend=codex to a running weiran
#      server, wait for status=idle, verify a turn ran. Validates weiran's
#      end-to-end wiring.
#
# Stage 2 needs a running weiran server with codex enabled. If
# WEIRAN_SERVER_URL / WEIRAN_AUTH_TOKEN aren't in the env we skip stage 2
# with a yellow note instead of failing — direct probe alone is enough to
# catch most regressions in CI.
#
# Exit codes:
#   0  — all stages green
#   1  — direct probe failed (codex protocol broke or codex misconfigured)
#   2  — server probe failed (weiran wiring broke)
#   10 — preflight failed (codex binary missing, no auth, wrong shell)

set -euo pipefail

GREEN=$'\033[32m'; RED=$'\033[31m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; CLR=$'\033[0m'

ok()    { printf "${GREEN}✓${CLR} %s\n" "$*"; }
fail()  { printf "${RED}✗${CLR} %s\n" "$*" >&2; }
note()  { printf "${YELLOW}!${CLR} %s\n" "$*"; }
hdr()   { printf "\n${BOLD}=== %s ===${CLR}\n" "$*"; }

CODEX_BIN="${CODEX_BIN:-/opt/homebrew/bin/codex}"
CODEX_MODEL="${CODEX_MODEL:-gpt-5.5}"
CODEX_PROMPT="${CODEX_PROMPT:-reply with one word: ok}"

# ── Preflight ──────────────────────────────────────────────────────────────

hdr "preflight"

if [[ ! -x "$CODEX_BIN" ]]; then
  fail "codex binary not found at $CODEX_BIN"
  cat <<EOF >&2

  Install codex:
    npm install -g @openai/codex

  Then re-run with:
    CODEX_BIN=\$(which codex) $0
EOF
  exit 10
fi
ok "codex binary: $CODEX_BIN ($("$CODEX_BIN" --version 2>/dev/null | head -1))"

if [[ ! -d "$HOME/.codex" ]]; then
  fail "codex not authenticated ($HOME/.codex missing)"
  cat <<EOF >&2

  Authenticate codex:
    codex login

EOF
  exit 10
fi
ok "codex auth dir: $HOME/.codex"

if ! command -v python3 >/dev/null 2>&1; then
  fail "python3 required for jsonrpc probe"
  exit 10
fi
ok "python3: $(python3 --version 2>&1)"

# ── Stage 1: direct app-server probe ───────────────────────────────────────

hdr "stage 1 — direct codex app-server probe"

PROBE=$(mktemp -t codex-smoke.XXXXXX.py)
trap 'rm -f "$PROBE"' EXIT

cat <<'PY' > "$PROBE"
import json, os, queue, subprocess, sys, threading, time

bin_path = os.environ["CODEX_BIN"]
model    = os.environ["CODEX_MODEL"]
prompt   = os.environ["CODEX_PROMPT"]

proc = subprocess.Popen(
    [bin_path, "app-server", "--listen", "stdio://"],
    stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
)

stderr_lines = []
def drain_stderr():
    for line in iter(proc.stderr.readline, b""):
        stderr_lines.append(line.decode(errors="replace").rstrip())
threading.Thread(target=drain_stderr, daemon=True).start()

q = queue.Queue()
def drain_stdout():
    for line in iter(proc.stdout.readline, b""):
        try:
            q.put(json.loads(line.decode()))
        except Exception:
            pass
threading.Thread(target=drain_stdout, daemon=True).start()

def send(obj):
    proc.stdin.write((json.dumps(obj) + "\n").encode())
    proc.stdin.flush()

def wait_id(rid, timeout=15):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            obj = q.get(timeout=0.5)
        except queue.Empty:
            continue
        if obj.get("id") == rid:
            return obj
    return None

def stderr_tail():
    return "\n".join(stderr_lines[-20:])

failed = False
try:
    send({"jsonrpc":"2.0","id":1,"method":"initialize","params":{
        "clientInfo":{"name":"weiran-smoke","title":"weiran smoke","version":"0.0.1"}
    }})
    init = wait_id(1, timeout=10)
    if not init or "result" not in init:
        print("FAIL initialize:", init, file=sys.stderr)
        print(stderr_tail(), file=sys.stderr)
        sys.exit(1)
    print("OK initialize ->", init["result"].get("userAgent", "?"))

    send({"jsonrpc":"2.0","method":"initialized","params":{}})

    send({"jsonrpc":"2.0","id":2,"method":"thread/start","params":{
        "cwd": os.getcwd(),
        "model": model,
        "approvalPolicy": "never",
    }})
    th = wait_id(2, timeout=15)
    if not th or "result" not in th:
        print("FAIL thread/start:", th, file=sys.stderr)
        print(stderr_tail(), file=sys.stderr)
        sys.exit(1)
    thread_id = th["result"]["thread"]["id"]
    print("OK thread/start ->", thread_id)

    send({"jsonrpc":"2.0","id":3,"method":"turn/start","params":{
        "threadId": thread_id,
        "input": [{"type":"text","text": prompt}],
    }})
    ts = wait_id(3, timeout=30)
    if not ts or "result" not in ts:
        print("FAIL turn/start:", ts, file=sys.stderr)
        print(stderr_tail(), file=sys.stderr)
        sys.exit(1)
    print("OK turn/start ->", ts["result"]["turn"]["id"])

    deadline = time.time() + 60
    saw_completed = False
    saw_message_or_completed = False
    while time.time() < deadline and not saw_completed:
        try:
            obj = q.get(timeout=0.5)
        except queue.Empty:
            continue
        if obj.get("method") == "turn/completed":
            saw_completed = True
            params = obj.get("params", {})
            status = params.get("turn", {}).get("status")
            print("OK turn/completed ->", status, "in", params.get("turn", {}).get("durationMs"), "ms")
        elif obj.get("method") == "item/agentMessage/delta":
            saw_message_or_completed = True
        elif obj.get("method") == "item/completed":
            saw_message_or_completed = True

    if not saw_completed:
        print("FAIL no turn/completed within 60s", file=sys.stderr)
        print(stderr_tail(), file=sys.stderr)
        sys.exit(1)
finally:
    proc.terminate()
    try:
        proc.wait(timeout=3)
    except Exception:
        proc.kill()

sys.exit(0)
PY

if CODEX_BIN="$CODEX_BIN" CODEX_MODEL="$CODEX_MODEL" CODEX_PROMPT="$CODEX_PROMPT" python3 "$PROBE"; then
  ok "stage 1 PASSED — direct codex protocol works on this box"
else
  fail "stage 1 FAILED — codex protocol broken on this box"
  exit 1
fi

# ── Stage 2: weiran server probe ───────────────────────────────────────────

hdr "stage 2 — weiran server probe"

if [[ -z "${WEIRAN_SERVER_URL:-}" || -z "${WEIRAN_AUTH_TOKEN:-}" ]]; then
  note "WEIRAN_SERVER_URL or WEIRAN_AUTH_TOKEN not set — skipping server probe"
  note "    (run with WEIRAN_SERVER_URL=http://127.0.0.1:8090 WEIRAN_AUTH_TOKEN=... to enable)"
  echo
  ok "smoke complete (stage 1 only)"
  exit 0
fi

H_AUTH="Authorization: Bearer ${WEIRAN_AUTH_TOKEN}"

probe_health() {
  local code
  code=$(curl -s -o /dev/null -w "%{http_code}" "${WEIRAN_SERVER_URL%/}/api/health" || echo "000")
  [[ "$code" == "200" ]]
}

if ! probe_health; then
  fail "weiran server unreachable at $WEIRAN_SERVER_URL"
  exit 2
fi
ok "weiran server reachable"

# Check codex feature is enabled by inspecting metrics endpoint (public)
if curl -fs "${WEIRAN_SERVER_URL%/}/api/codex/metrics" >/dev/null 2>&1; then
  ok "codex metrics endpoint live"
else
  note "codex metrics endpoint unavailable — server may be older than Round 5"
fi

# Create a session with backend=codex
SESS_RESP=$(curl -s -X POST "${WEIRAN_SERVER_URL%/}/api/sessions" \
  -H "$H_AUTH" \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"codex-smoke-$(date +%s)\",
    \"backend\": \"codex\",
    \"model\": \"codex/${CODEX_MODEL}\",
    \"project\": \"$(pwd)\",
    \"initial_message\": \"$CODEX_PROMPT\"
  }")

SESS_ID=$(printf '%s' "$SESS_RESP" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('id') or d.get('session_id') or '')" 2>/dev/null || true)

if [[ -z "$SESS_ID" ]]; then
  fail "session create failed: $SESS_RESP"
  exit 2
fi
ok "session created: $SESS_ID"

# Poll for status=idle (turn finished) up to 60s
deadline=$(( $(date +%s) + 60 ))
status=""
while [[ $(date +%s) -lt $deadline ]]; do
  STATUS_RESP=$(curl -s -H "$H_AUTH" "${WEIRAN_SERVER_URL%/}/api/sessions/${SESS_ID}" || echo "")
  status=$(printf '%s' "$STATUS_RESP" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('status',''))" 2>/dev/null || true)
  if [[ "$status" == "idle" || "$status" == "error" || "$status" == "stopped" ]]; then
    break
  fi
  sleep 1
done

if [[ "$status" != "idle" ]]; then
  fail "session did not reach idle (final status: $status)"
  printf "%s\n" "$STATUS_RESP" >&2
  exit 2
fi
ok "session reached idle (turn completed)"

# Inspect metrics — codex_handshake.ok_total should be > 0
METRICS=$(curl -s "${WEIRAN_SERVER_URL%/}/api/codex/metrics" || echo "")
HS_OK=$(printf '%s' "$METRICS" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d['codex_handshake']['ok_total'])" 2>/dev/null || echo 0)
if [[ "$HS_OK" -gt 0 ]]; then
  ok "codex handshake counter incremented: ok_total=$HS_OK"
else
  note "codex handshake counter is 0 — endpoint may be stale or session ran on cc"
fi

# Optional: clean up (delete the smoke session)
if [[ "${KEEP_SESSION:-0}" != "1" ]]; then
  curl -s -X DELETE -H "$H_AUTH" "${WEIRAN_SERVER_URL%/}/api/sessions/${SESS_ID}" >/dev/null || true
  ok "smoke session deleted"
fi

hdr "result"
ok "all stages PASSED"
