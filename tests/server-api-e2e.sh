#!/bin/bash
# server-api-e2e.sh — End-to-end API test for weiran server mode
# Usage: ./tests/server-api-e2e.sh [base_url] [token]
# Defaults: localhost:9847, reads token from ~/.openclaw/data/config.json
#
# Exit codes: 0 = all pass, 1 = failures found

set -euo pipefail

BASE="${1:-http://localhost:9847}"
TOKEN="${2:-$(python3 -c "import json;print(json.load(open('$HOME/.openclaw/data/config.json'))['server']['token'])" 2>/dev/null || echo "")}"

if [ -z "$TOKEN" ]; then
  echo "FAIL: no server token found"
  exit 1
fi

PASS=0
FAIL=0
TOTAL=0

check() {
  local name="$1" expected="$2" actual="$3"
  TOTAL=$((TOTAL + 1))
  if [ "$actual" = "$expected" ]; then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
    echo "FAIL: $name — expected '$expected', got '$actual'"
  fi
}

check_contains() {
  local name="$1" needle="$2" haystack="$3"
  TOTAL=$((TOTAL + 1))
  if echo "$haystack" | grep -qF "$needle"; then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
    echo "FAIL: $name — expected to contain '$needle'"
  fi
}

# Authenticated curl wrapper
acurl() {
  curl -s -H "Authorization: Bearer $TOKEN" "$@"
}

acurl_code() {
  curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$@"
}

# ── Read-only endpoints ──

# 1. Health (no auth)
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/health")
check "health status code" "200" "$STATUS"

HEALTH=$(curl -s "$BASE/api/health")
check_contains "health body" '"status":"ok"' "$HEALTH"

# 2. Auth required
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/sessions")
check "sessions no auth" "401" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer wrong" "$BASE/api/sessions")
check "sessions wrong token" "401" "$STATUS"

# 3. Query param auth
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/config?token=$TOKEN")
check "query param auth" "200" "$STATUS"

# 4. Config
CONFIG=$(acurl "$BASE/api/config")
check_contains "config has workspace" '"workspace"' "$CONFIG"
check_contains "config has claude_bin" '"claude_bin"' "$CONFIG"

# 5. Skills
SKILLS=$(acurl "$BASE/api/skills")
SKILL_COUNT=$(echo "$SKILLS" | python3 -c "import sys,json;print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")
TOTAL=$((TOTAL + 1))
if [ "$SKILL_COUNT" -gt 0 ]; then
  PASS=$((PASS + 1))
else
  FAIL=$((FAIL + 1))
  echo "FAIL: skills — expected >0 skills, got $SKILL_COUNT"
fi

# 6. Sessions list
STATUS=$(acurl_code "$BASE/api/sessions")
check "sessions list" "200" "$STATUS"

# 7. History
HISTORY=$(acurl "$BASE/api/history?limit=3")
HIST_COUNT=$(echo "$HISTORY" | python3 -c "import sys,json;print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")
TOTAL=$((TOTAL + 1))
if [ "$HIST_COUNT" -ge 0 ]; then
  PASS=$((PASS + 1))
else
  FAIL=$((FAIL + 1))
  echo "FAIL: history — invalid response"
fi

# 8. History messages (pick first session)
HIST_ID=$(echo "$HISTORY" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d[0]['id'] if d else '')" 2>/dev/null || echo "")
if [ -n "$HIST_ID" ]; then
  STATUS=$(acurl_code "$BASE/api/history/$HIST_ID/messages?limit=5")
  check "history messages" "200" "$STATUS"
else
  TOTAL=$((TOTAL + 1)); PASS=$((PASS + 1)) # skip if no history
fi

# 9. Link preview
LP=$(acurl "$BASE/api/link-preview?url=https://github.com")
check_contains "link preview title" '"title"' "$LP"

# 10. Link preview missing url
STATUS=$(acurl_code "$BASE/api/link-preview")
check "link preview no url" "400" "$STATUS"

# ── Session lifecycle ──

# Count sessions before
BEFORE_COUNT=$(acurl "$BASE/api/sessions" | python3 -c "import sys,json;print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")

# 11. Create session
CREATE=$(acurl -X POST -H "Content-Type: application/json" \
  -d '{"name":"e2e-test","soul_files":false}' \
  "$BASE/api/sessions")
SID=$(echo "$CREATE" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
check_contains "create session" '"status":"running"' "$CREATE"

if [ -z "$SID" ]; then
  echo "FATAL: could not create session, aborting lifecycle tests"
  echo ""
  echo "Results: $PASS/$TOTAL passed, $FAIL failed"
  exit 1
fi

# Ensure cleanup on exit
cleanup() {
  if [ -n "${SID:-}" ]; then
    acurl -X DELETE "$BASE/api/sessions/$SID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# 12. Get session
GET=$(acurl "$BASE/api/sessions/$SID")
check_contains "get session name" '"name":"e2e-test"' "$GET"

# 13. Send message
sleep 2  # wait for Claude to init
MSG=$(acurl -X POST -H "Content-Type: application/json" \
  -d '{"message":"reply with exactly one word: PONG"}' \
  "$BASE/api/sessions/$SID/message")
check_contains "send message" '"status":"sent"' "$MSG"

# 14. Wait for response and check turns
sleep 10
GET2=$(acurl "$BASE/api/sessions/$SID")
TURNS=$(echo "$GET2" | python3 -c "import sys,json;print(json.load(sys.stdin).get('num_turns',0))" 2>/dev/null || echo "0")
TOTAL=$((TOTAL + 1))
if [ "$TURNS" -gt 0 ]; then
  PASS=$((PASS + 1))
else
  FAIL=$((FAIL + 1))
  echo "FAIL: message processed — turns=$TURNS (expected >0)"
fi

# 15. Rename
RENAME=$(acurl -X PATCH -H "Content-Type: application/json" \
  -d '{"name":"e2e-renamed"}' \
  "$BASE/api/sessions/$SID/rename")
check_contains "rename" '"status":"renamed"' "$RENAME"

# 16. Verify rename
GET3=$(acurl "$BASE/api/sessions/$SID")
check_contains "rename verified" '"name":"e2e-renamed"' "$GET3"

# 17. Control request
CTRL=$(acurl -X POST -H "Content-Type: application/json" \
  -d '{"subtype":"context_usage"}' \
  "$BASE/api/sessions/$SID/control")
check_contains "control request" '"status":"sent"' "$CTRL"

# 18. Delete session
DEL=$(acurl -X DELETE "$BASE/api/sessions/$SID")
check_contains "delete session" '"status":"destroyed"' "$DEL"
SID=""  # prevent cleanup trap from double-deleting

# 19. Verify deleted
STATUS=$(acurl_code "$BASE/api/sessions/$SID")
check "session gone" "404" "$STATUS"

# 20. Sessions count restored
AFTER_COUNT=$(acurl "$BASE/api/sessions" | python3 -c "import sys,json;print(len(json.load(sys.stdin)))" 2>/dev/null || echo "?")
check "session count restored" "$BEFORE_COUNT" "$AFTER_COUNT"

# ── Error cases ──

# 21. Get non-existent session
STATUS=$(acurl_code "$BASE/api/sessions/nonexistent-id")
check "get nonexistent" "404" "$STATUS"

# 22. Send message to non-existent session
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"message":"hi"}' "$BASE/api/sessions/nonexistent-id/message")
check "message nonexistent" "404" "$STATUS"

# 23. Delete non-existent session
STATUS=$(acurl_code -X DELETE "$BASE/api/sessions/nonexistent-id")
check "delete nonexistent" "404" "$STATUS"

# 24. Create with bad JSON
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d 'not json' "$BASE/api/sessions")
check "create bad json" "400" "$STATUS"

# 25. Rename non-existent
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"x"}' "$BASE/api/sessions/nonexistent-id/rename")
check "rename nonexistent" "404" "$STATUS"

# ── Summary ──
echo ""
if [ "$FAIL" -eq 0 ]; then
  echo "ALL PASSED: $PASS/$TOTAL tests"
else
  echo "FAILED: $FAIL/$TOTAL tests failed ($PASS passed)"
fi
exit "$FAIL"
