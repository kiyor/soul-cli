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
