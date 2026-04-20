package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractToolPath(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		input    string
		expected string
	}{
		{"Read file_path", "Read", `{"file_path":"/tmp/a.md"}`, "/tmp/a.md"},
		{"Edit file_path", "Edit", `{"file_path":"/tmp/b.md","old_string":"x","new_string":"y"}`, "/tmp/b.md"},
		{"Grep path", "Grep", `{"pattern":"TODO","path":"/src"}`, "/src"},
		{"Glob path", "Glob", `{"pattern":"*.go","path":"/x"}`, "/x"},
		{"Bash no path", "Bash", `{"command":"ls"}`, ""},
		{"empty input", "Read", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractToolPath(tc.tool, json.RawMessage(tc.input))
			if got != tc.expected {
				t.Errorf("got %q want %q", got, tc.expected)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// no **
		{"*.go", "/tmp/main.go", true}, // basename match
		{"*.go", "/tmp/main.py", false},
		{"/tmp/*.md", "/tmp/a.md", true},

		// leading **
		{"**/*.jsonl", "/a/b/c/foo.jsonl", true},
		{"**/*.jsonl", "foo.jsonl", true},
		{"**/feedback_*.md", "/home/k/memory/topics/feedback_x.md", true},
		{"**/feedback_*.md", "/home/k/memory/topics/other.md", false},

		// deeply nested
		{"**/.claude/projects/**/*.jsonl", "/Users/k/.claude/projects/abc/s.jsonl", true},
		{"**/.claude/projects/**/*.jsonl", "/Users/k/.claude/projects/abc/def/s.jsonl", true},
		{"**/.claude/projects/**/*.jsonl", "/Users/k/.claude/notes/s.jsonl", false},

		// daily note pattern
		{"**/memory/20*-*.md", "/Users/k/memory/2026-04-17.md", true},
		{"**/memory/20*-*.md", "/Users/k/memory/topics/feedback_x.md", false},
	}
	for _, tc := range cases {
		got := globMatch(tc.pattern, tc.path)
		if got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestLoadToolHookConfig_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	cfg, err := loadToolHookConfig(filepath.Join(tmp, "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if cfg == nil || len(cfg.Rules) != 0 {
		t.Errorf("expected empty config for missing file")
	}
}

func TestLoadToolHookConfig_Defaults(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "rules.yaml")
	os.WriteFile(p, []byte(`rules:
  - id: r1
    match: ["*.md"]
    inject: "hello"
`), 0644)
	cfg, err := loadToolHookConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Budget != 1500 {
		t.Errorf("global budget default: got %d want 1500", cfg.Budget)
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(cfg.Rules))
	}
	r := cfg.Rules[0]
	if r.Budget != 500 {
		t.Errorf("rule budget default: got %d", r.Budget)
	}
	if r.Priority != 50 {
		t.Errorf("rule priority default: got %d", r.Priority)
	}
	if r.Dedupe != "per_file" {
		t.Errorf("dedupe default: got %q", r.Dedupe)
	}
}

func TestLoadToolHookConfig_PrioritySort(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "rules.yaml")
	os.WriteFile(p, []byte(`rules:
  - id: low
    match: ["*"]
    inject: a
    priority: 10
  - id: high
    match: ["*"]
    inject: b
    priority: 100
  - id: mid
    match: ["*"]
    inject: c
    priority: 50
`), 0644)
	cfg, err := loadToolHookConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rules[0].ID != "high" || cfg.Rules[1].ID != "mid" || cfg.Rules[2].ID != "low" {
		t.Errorf("priority sort wrong: %v", cfg.Rules)
	}
}

func TestToolMatches(t *testing.T) {
	cases := []struct {
		rule ToolHookRule
		tool string
		want bool
	}{
		{ToolHookRule{Tools: nil}, "Read", true},
		{ToolHookRule{Tools: []string{"Read"}}, "Read", true},
		{ToolHookRule{Tools: []string{"Read"}}, "Edit", false},
		{ToolHookRule{Tools: []string{"Read", "Edit"}}, "Edit", true},
		{ToolHookRule{Tools: []string{"read"}}, "Read", true}, // case-insensitive
	}
	for _, tc := range cases {
		got := tc.rule.toolMatches(tc.tool)
		if got != tc.want {
			t.Errorf("tool=%s tools=%v: got %v want %v", tc.tool, tc.rule.Tools, got, tc.want)
		}
	}
}

func TestRunToolHook_NoMatch_WritesAuditRow(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	// empty config
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	// invoke hook with input that matches no rule
	input := ToolHookInput{
		SessionID: "test-sess",
		CWD:       "/tmp",
		ToolName:  "Read",
		ToolInput: json.RawMessage(`{"file_path":"/no/match/file.txt"}`),
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	// capture stdout (expect empty — no injection)
	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout

	buf := make([]byte, 1024)
	n, _ := pr.Read(buf)
	out := string(buf[:n])
	if strings.Contains(out, "additionalContext") {
		t.Errorf("expected no injection, got %q", out)
	}

	// verify audit row was written
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var cnt int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE session_id='test-sess'`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("expected 1 audit row, got %d", cnt)
	}
}

func TestRunToolHook_Match_InjectsAndAudits(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	defer func() { dbPath = origDB }()

	// write rule matching /tmp/test.md
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()
	rulePath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(rulePath, []byte(`rules:
  - id: test_rule
    tools: [Read]
    match: ["**/*.md"]
    inject: "hello from rule"
    dedupe: per_file
`), 0644)

	input := ToolHookInput{
		SessionID: "s1",
		ToolName:  "Read",
		ToolInput: json.RawMessage(`{"file_path":"/tmp/test.md"}`),
	}
	payload, _ := json.Marshal(input)

	// first call — should inject
	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdin = origStdin
	os.Stdout = origStdout
	buf := make([]byte, 2048)
	n, _ := pr.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "hello from rule") {
		t.Errorf("first call: expected injection, got %q", out)
	}
	if !strings.Contains(out, "hookSpecificOutput") || !strings.Contains(out, "additionalContext") {
		t.Errorf("first call: missing hookSpecificOutput/additionalContext wrapper: %q", out)
	}

	// second call — dedupe per_file should skip injection
	r2, w2, _ := os.Pipe()
	w2.Write(payload)
	w2.Close()
	os.Stdin = r2
	pr2, pw2, _ := os.Pipe()
	os.Stdout = pw2
	runToolHook()
	pw2.Close()
	os.Stdin = origStdin
	os.Stdout = origStdout
	buf2 := make([]byte, 2048)
	n2, _ := pr2.Read(buf2)
	out2 := string(buf2[:n2])
	if strings.Contains(out2, "hello from rule") {
		t.Errorf("second call: expected dedupe skip, got %q", out2)
	}

	// verify audit: 1 injected + 1 dedupe skip
	db, _ := openDB()
	defer db.Close()
	var injected, skipped int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE rule_id='test_rule' AND injected=1`).Scan(&injected)
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE rule_id='test_rule' AND skip_reason='dedupe'`).Scan(&skipped)
	if injected != 1 {
		t.Errorf("injected rows: got %d want 1", injected)
	}
	if skipped != 1 {
		t.Errorf("dedupe rows: got %d want 1", skipped)
	}
}

func TestQueryToolHookStats(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "stats.db")
	defer func() { dbPath = origDB }()

	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// seed a few rows
	rows := []toolHookAuditRow{
		{Timestamp: "2026-04-17T10:00:00-07:00", SessionID: "s1", ToolName: "Read", Path: "/a.md", RuleID: "r1", Injected: true, InjectionSize: 100, LatencyMS: 5},
		{Timestamp: "2026-04-17T10:01:00-07:00", SessionID: "s1", ToolName: "Read", Path: "/a.md", RuleID: "r1", Injected: false, SkipReason: "dedupe", LatencyMS: 3},
		{Timestamp: "2026-04-17T10:02:00-07:00", SessionID: "s1", ToolName: "Edit", Path: "/b.md", LatencyMS: 2},
	}
	for _, r := range rows {
		writeToolHookAudit(db, r)
	}

	stats, err := queryToolHookStats(db, 7)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalCalls != 3 {
		t.Errorf("total: got %d want 3", stats.TotalCalls)
	}
	if stats.InjectedCalls != 1 {
		t.Errorf("injected: got %d want 1", stats.InjectedCalls)
	}
	if stats.MatchedCalls != 2 {
		t.Errorf("matched: got %d want 2", stats.MatchedCalls)
	}
	if stats.ByTool["Read"] != 2 || stats.ByTool["Edit"] != 1 {
		t.Errorf("by_tool: got %v", stats.ByTool)
	}
	if stats.SkipBreakdown["dedupe"] != 1 {
		t.Errorf("skip: got %v", stats.SkipBreakdown)
	}
	if r1 := stats.ByRule["r1"]; r1.Calls != 2 || r1.Injected != 1 {
		t.Errorf("r1: got %+v", r1)
	}
}

// ── UserPromptSubmit dispatch tests ──

func TestEventMatches(t *testing.T) {
	cases := []struct {
		name   string
		rule   ToolHookRule
		event  string
		expect bool
	}{
		{"empty events defaults to PreToolUse", ToolHookRule{}, HookEventPreToolUse, true},
		{"empty events does not match UserPromptSubmit", ToolHookRule{}, HookEventUserPromptSubmit, false},
		{"explicit match", ToolHookRule{Events: []string{"UserPromptSubmit"}}, HookEventUserPromptSubmit, true},
		{"explicit no-match", ToolHookRule{Events: []string{"UserPromptSubmit"}}, HookEventPreToolUse, false},
		{"case-insensitive", ToolHookRule{Events: []string{"userpromptsubmit"}}, HookEventUserPromptSubmit, true},
		{"multi-event list", ToolHookRule{Events: []string{"PreToolUse", "UserPromptSubmit"}}, HookEventUserPromptSubmit, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.rule.eventMatches(tc.event)
			if got != tc.expect {
				t.Errorf("got %v want %v", got, tc.expect)
			}
		})
	}
}

func TestMatchesPromptRegex(t *testing.T) {
	cases := []struct {
		name     string
		prompt   string
		patterns []string
		want     bool
	}{
		{"empty prompt", "", []string{"foo"}, false},
		{"empty patterns", "hello", nil, false},
		{"simple match", "请记住这件事", []string{"记住|记下"}, true},
		{"case-insensitive flag", "REMEMBER this", []string{"(?i)remember this"}, true},
		{"case-sensitive mismatch", "REMEMBER this", []string{"remember this"}, false},
		{"no match", "天气真好", []string{"记住|记下"}, false},
		{"bad regex skipped (no crash)", "记住", []string{"[invalid", "记住"}, true},
		{"bad regex no match", "天气", []string{"[invalid"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchesPromptRegex(tc.prompt, tc.patterns)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestRunUserPromptSubmitHook_Match(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "ups.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	os.WriteFile(filepath.Join(dir, "tool-hooks.yaml"), []byte(`rules:
  - id: remember_signal
    events: [UserPromptSubmit]
    match_prompt:
      - '记住|记下|remember this'
    inject: "write to file, do not just reply"
    dedupe: never
`), 0644)

	input := ToolHookInput{
		SessionID:     "ups-sess",
		HookEventName: "UserPromptSubmit",
		Prompt:        "主人说 请记住 以后都这样",
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout

	buf := make([]byte, 2048)
	n, _ := pr.Read(buf)
	out := string(buf[:n])

	if !strings.Contains(out, "write to file") {
		t.Errorf("expected injection, got %q", out)
	}
	if !strings.Contains(out, `"hookEventName":"UserPromptSubmit"`) {
		t.Errorf("expected UserPromptSubmit event name in output, got %q", out)
	}

	db, _ := openDB()
	defer db.Close()
	var n1 int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE rule_id='remember_signal' AND injected=1 AND event_name='UserPromptSubmit'`).Scan(&n1)
	if n1 != 1 {
		t.Errorf("expected 1 UserPromptSubmit audit row, got %d", n1)
	}
}

func TestRunUserPromptSubmitHook_NoMatch(t *testing.T) {
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "ups-nomatch.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	os.WriteFile(filepath.Join(dir, "tool-hooks.yaml"), []byte(`rules:
  - id: remember_signal
    events: [UserPromptSubmit]
    match_prompt: ['记住|记下']
    inject: "write to file"
`), 0644)

	input := ToolHookInput{
		SessionID:     "ups-nomatch",
		HookEventName: "UserPromptSubmit",
		Prompt:        "今天天气怎么样",
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout

	buf := make([]byte, 1024)
	n, _ := pr.Read(buf)
	out := string(buf[:n])
	if strings.Contains(out, "additionalContext") {
		t.Errorf("expected no injection, got %q", out)
	}

	// Observability row still written (no rule matched)
	db, _ := openDB()
	defer db.Close()
	var cnt int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit WHERE session_id='ups-nomatch' AND event_name='UserPromptSubmit'`).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("expected 1 observability row, got %d", cnt)
	}
}

func TestEventDispatch_BackwardCompat_NoEventName(t *testing.T) {
	// Payloads without hook_event_name must still work as PreToolUse.
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "bc.db")
	defer func() { dbPath = origDB }()

	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	os.WriteFile(filepath.Join(dir, "tool-hooks.yaml"), []byte(`rules:
  - id: legacy_rule
    tools: [Read]
    match: ["**/*.md"]
    inject: "legacy fires"
`), 0644)

	input := ToolHookInput{
		SessionID: "bc-sess",
		ToolName:  "Read",
		ToolInput: json.RawMessage(`{"file_path":"/tmp/x.md"}`),
		// HookEventName intentionally empty
	}
	payload, _ := json.Marshal(input)

	r, w, _ := os.Pipe()
	w.Write(payload)
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	pr, pw, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = pw
	runToolHook()
	pw.Close()
	os.Stdout = origStdout

	buf := make([]byte, 2048)
	n, _ := pr.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "legacy fires") {
		t.Errorf("expected legacy rule injection, got %q", out)
	}
}

func TestPromptDigest(t *testing.T) {
	cases := []struct {
		in  string
		n   int
		out string
	}{
		{"", 10, ""},
		{"short", 10, "short"},
		{"line1\nline2", 100, "line1 line2"},
		{"abcdefghijklmnop", 5, "abcde…"},
		{"中文测试内容", 3, "中文测…"},
	}
	for _, tc := range cases {
		got := promptDigest(tc.in, tc.n)
		if got != tc.out {
			t.Errorf("promptDigest(%q, %d) = %q want %q", tc.in, tc.n, got, tc.out)
		}
	}
}
