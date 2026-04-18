package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ── PreToolUse hook: path-aware system-reminder injection ──
//
// Claude Code invokes the hook with a JSON payload on stdin. For PreToolUse
// it contains: {"tool_name": "...", "tool_input": {...}, "session_id": "...", "cwd": "..."}
// Any stdout the hook produces is injected as a system-reminder before Claude
// processes the tool result. Exit 0 = allow, non-zero = block (we never block;
// injection is advisory only).
//
// Every invocation writes one row to tool_hook_audit. The table doubles as
// dedup state (per_session / per_file): dedup checks query WHERE injected=1.

// ToolHookInput mirrors the PreToolUse payload Claude Code sends via stdin.
type ToolHookInput struct {
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	HookEventName  string          `json:"hook_event_name"`
	TranscriptPath string          `json:"transcript_path"`
}

// ToolHookRule is one YAML rule definition.
type ToolHookRule struct {
	ID        string   `yaml:"id"`
	Match     []string `yaml:"match"`     // glob patterns against target path
	Tools     []string `yaml:"tools"`     // Read / Edit / Write / Grep / Glob; empty = any
	Inject    string   `yaml:"inject"`    // system-reminder body (trimmed)
	Dedupe    string   `yaml:"dedupe"`    // never | per_session | per_file (default per_file)
	Budget    int      `yaml:"budget"`    // per-rule max chars; 0 = 500 default
	Priority  int      `yaml:"priority"`  // higher fires first; 0 = 50 default
	Disabled  bool     `yaml:"disabled"`
}

// ToolHookConfig is the top-level YAML doc.
type ToolHookConfig struct {
	Budget int            `yaml:"budget"` // global max chars per hook call; 0 = 1500 default
	Rules  []ToolHookRule `yaml:"rules"`
}

// defaultToolHookConfigPath returns the rules YAML location.
// Override with <APPNAME>_TOOL_HOOKS env var.
func defaultToolHookConfigPath() string {
	if v := os.Getenv(strings.ToUpper(appName) + "_TOOL_HOOKS"); v != "" {
		return v
	}
	return filepath.Join(workspace, "tool-hooks.yaml")
}

// extractToolPath pulls the file_path / path from tool_input based on tool name.
// Returns "" if the tool has no path concept we care about.
func extractToolPath(tool string, input json.RawMessage) string {
	var probe struct {
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		NotebookPath string `json:"notebook_path"`
	}
	_ = json.Unmarshal(input, &probe)
	switch {
	case probe.FilePath != "":
		return probe.FilePath
	case probe.NotebookPath != "":
		return probe.NotebookPath
	case probe.Path != "":
		return probe.Path
	}
	return ""
}

// matchesGlob checks if path matches any of the glob patterns. Supports:
//   - leading ~ expansion
//   - ** for deep match (via prefix/suffix split)
//   - filepath.Match semantics for single segment
func matchesGlob(path string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	normalized := path
	if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(normalized, home) {
		// keep as-is; patterns may use either ~ or absolute
	}
	for _, pat := range patterns {
		if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(pat, "~/") {
			pat = filepath.Join(home, pat[2:])
		}
		if globMatch(pat, normalized) {
			return true
		}
	}
	return false
}

// globMatch translates a glob pattern to a regex and matches against s.
//   ** → matches any sequence of characters including /
//   *  → matches any sequence except /
//   ?  → matches a single non-/ char
// Everything else is quoted literally. Pattern must match the full string.
//
// Special case: when the pattern starts with "**/", zero path segments
// (i.e. plain basename match) are also accepted — so "**/*.jsonl" matches
// both "a/b/foo.jsonl" and "foo.jsonl".
func globMatch(pattern, s string) bool {
	rx := compileGlobRegex(pattern)
	if rx == nil {
		return false
	}
	if rx.MatchString(s) {
		return true
	}
	// Convenience: if the pattern has no slash at all (e.g. "*.go"),
	// also try it against the basename.
	if !strings.Contains(pattern, "/") {
		return rx.MatchString(filepath.Base(s))
	}
	return false
}

var globCache sync.Map // pattern → *regexp.Regexp

func compileGlobRegex(pattern string) *regexp.Regexp {
	if v, ok := globCache.Load(pattern); ok {
		return v.(*regexp.Regexp)
	}
	rx := buildGlobRegex(pattern)
	globCache.Store(pattern, rx)
	return rx
}

func buildGlobRegex(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	// Special prefix: "**/" allows zero segments. We express this in the
	// regex as "(?:.*/)?" instead of ".*/" so it can match the bare basename.
	rest := pattern
	if strings.HasPrefix(rest, "**/") {
		b.WriteString("(?:.*/)?")
		rest = rest[3:]
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch c {
		case '*':
			if i+1 < len(rest) && rest[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	rx, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return rx
}

// loadToolHookConfig reads and parses the YAML. Missing file → empty config.
func loadToolHookConfig(path string) (*ToolHookConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ToolHookConfig{}, nil
		}
		return nil, err
	}
	var cfg ToolHookConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Budget == 0 {
		cfg.Budget = 1500
	}
	for i := range cfg.Rules {
		if cfg.Rules[i].Budget == 0 {
			cfg.Rules[i].Budget = 500
		}
		if cfg.Rules[i].Priority == 0 {
			cfg.Rules[i].Priority = 50
		}
		if cfg.Rules[i].Dedupe == "" {
			cfg.Rules[i].Dedupe = "per_file"
		}
	}
	sort.SliceStable(cfg.Rules, func(i, j int) bool {
		return cfg.Rules[i].Priority > cfg.Rules[j].Priority
	})
	return &cfg, nil
}

// toolMatches returns true if the rule's Tools whitelist includes this tool.
// Empty Tools = match any.
func (r *ToolHookRule) toolMatches(tool string) bool {
	if len(r.Tools) == 0 {
		return true
	}
	for _, t := range r.Tools {
		if strings.EqualFold(t, tool) {
			return true
		}
	}
	return false
}

// isDeduped checks the audit table for a prior successful injection of this
// rule, scoped by dedupe policy.
func isDeduped(db *sql.DB, rule ToolHookRule, sessionID, path string) bool {
	switch rule.Dedupe {
	case "never":
		return false
	case "per_session":
		var n int
		db.QueryRow(`SELECT 1 FROM tool_hook_audit WHERE rule_id=? AND session_id=? AND injected=1 LIMIT 1`,
			rule.ID, sessionID).Scan(&n)
		return n == 1
	case "per_file":
		fallthrough
	default:
		var n int
		// per_file dedupe is session-scoped so a new session can re-injection
		// the same file — intentional: new session = new context.
		db.QueryRow(`SELECT 1 FROM tool_hook_audit WHERE rule_id=? AND session_id=? AND path=? AND injected=1 LIMIT 1`,
			rule.ID, sessionID, path).Scan(&n)
		return n == 1
	}
}

// writeToolHookAudit persists one row.
func writeToolHookAudit(db *sql.DB, row toolHookAuditRow) {
	if db == nil {
		return
	}
	_, err := db.Exec(`INSERT INTO tool_hook_audit
		(timestamp, session_id, cwd, tool_name, path, rule_id, injected, skip_reason, injection_size, budget_used, latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Timestamp, row.SessionID, row.CWD, row.ToolName, row.Path,
		row.RuleID, row.Injected, row.SkipReason, row.InjectionSize, row.BudgetUsed, row.LatencyMS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tool-hook] audit write failed: %v\n", err)
	}
}

type toolHookAuditRow struct {
	Timestamp     string
	SessionID     string
	CWD           string
	ToolName      string
	Path          string
	RuleID        string
	Injected      bool
	SkipReason    string
	InjectionSize int
	BudgetUsed    int
	LatencyMS     int64
}

// runToolHook is the entry point when weiran is invoked as a PreToolUse hook.
// Reads JSON from stdin, writes system-reminder (if any) to stdout, never errors
// (hook failures must not block the user's work).
func runToolHook() {
	start := time.Now()

	rawIn, err := io.ReadAll(os.Stdin)
	if err != nil {
		// silent failure — hook must not block
		return
	}
	var in ToolHookInput
	if err := json.Unmarshal(rawIn, &in); err != nil {
		return
	}

	path := extractToolPath(in.ToolName, in.ToolInput)
	sessionID := in.SessionID

	cfg, err := loadToolHookConfig(defaultToolHookConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tool-hook] config load error: %v\n", err)
		return
	}

	db, _ := openDB()
	if db != nil {
		defer db.Close()
	}

	audited := false // ensure we write at least one "no match" row for observability
	budgetRemaining := cfg.Budget
	var injections []string
	budgetUsedTotal := 0

	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.ID == "" {
			continue
		}
		if !rule.toolMatches(in.ToolName) {
			continue
		}
		if !matchesGlob(path, rule.Match) {
			continue
		}
		audited = true

		if db != nil && isDeduped(db, rule, sessionID, path) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				ToolName:   in.ToolName,
				Path:       path,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "dedupe",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		body := strings.TrimSpace(rule.Inject)
		if len(body) > rule.Budget {
			body = body[:rule.Budget]
		}
		if len(body) > budgetRemaining {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				ToolName:   in.ToolName,
				Path:       path,
				RuleID:     rule.ID,
				Injected:   false,
				SkipReason: "budget",
				LatencyMS:  time.Since(start).Milliseconds(),
			})
			continue
		}

		injections = append(injections, fmt.Sprintf("[rule:%s] %s", rule.ID, body))
		budgetUsedTotal += len(body)
		budgetRemaining -= len(body)

		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp:     time.Now().Format(time.RFC3339),
			SessionID:     sessionID,
			CWD:           in.CWD,
			ToolName:      in.ToolName,
			Path:          path,
			RuleID:        rule.ID,
			Injected:      true,
			InjectionSize: len(body),
			BudgetUsed:    budgetUsedTotal,
			LatencyMS:     time.Since(start).Milliseconds(),
		})
	}

	// Always record at least one "observed" row so stats reflect total Read volume,
	// not just matched ones. Empty rule_id = no rule fired.
	if !audited && db != nil {
		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp: time.Now().Format(time.RFC3339),
			SessionID: sessionID,
			CWD:       in.CWD,
			ToolName:  in.ToolName,
			Path:      path,
			LatencyMS: time.Since(start).Milliseconds(),
		})
	}

	if len(injections) > 0 {
		// Claude Code PreToolUse supports injecting text into Claude's context
		// via JSON output with hookSpecificOutput.additionalContext.
		// Plain stdout is shown only to the user via the transcript.
		body := strings.Join(injections, "\n\n")
		out := map[string]interface{}{
			"hookSpecificOutput": map[string]interface{}{
				"hookEventName":     "PreToolUse",
				"additionalContext": body,
			},
			"suppressOutput": true,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.Encode(out)
	}
}

// ── CLI: `weiran tool-hook stats` ──

// toolHookStats aggregates recent audit rows for diagnostics.
type toolHookStats struct {
	TotalCalls      int                      `json:"total_calls"`
	MatchedCalls    int                      `json:"matched_calls"`
	InjectedCalls   int                      `json:"injected_calls"`
	ByRule          map[string]toolHookRuleS `json:"by_rule"`
	ByTool          map[string]int           `json:"by_tool"`
	TopPaths        []toolHookPathStat       `json:"top_paths"`
	SkipBreakdown   map[string]int           `json:"skip_breakdown"`
	DaysQueried     int                      `json:"days_queried"`
	AvgLatencyMS    int64                    `json:"avg_latency_ms"`
}

type toolHookRuleS struct {
	Calls        int   `json:"calls"`
	Injected     int   `json:"injected"`
	AvgInjection int   `json:"avg_injection_bytes"`
	AvgLatencyMS int64 `json:"avg_latency_ms"`
}

type toolHookPathStat struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// queryToolHookStats aggregates the last N days.
func queryToolHookStats(db *sql.DB, days int) (*toolHookStats, error) {
	since := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	stats := &toolHookStats{
		ByRule:        map[string]toolHookRuleS{},
		ByTool:        map[string]int{},
		SkipBreakdown: map[string]int{},
		DaysQueried:   days,
	}

	// totals + latency
	row := db.QueryRow(`SELECT COUNT(*), SUM(injected), ROUND(AVG(latency_ms))
		FROM tool_hook_audit WHERE timestamp >= ?`, since)
	var latency sql.NullFloat64
	var injected sql.NullInt64
	row.Scan(&stats.TotalCalls, &injected, &latency)
	stats.InjectedCalls = int(injected.Int64)
	stats.AvgLatencyMS = int64(latency.Float64)

	// matched (rule_id != '')
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? AND rule_id != ''`, since).Scan(&stats.MatchedCalls)

	// by rule
	rows, err := db.Query(`SELECT rule_id, COUNT(*), SUM(injected),
		ROUND(AVG(CASE WHEN injected=1 THEN injection_size ELSE 0 END)),
		ROUND(AVG(latency_ms))
		FROM tool_hook_audit WHERE timestamp >= ? AND rule_id != ''
		GROUP BY rule_id ORDER BY COUNT(*) DESC`, since)
	if err == nil {
		for rows.Next() {
			var id string
			var s toolHookRuleS
			var injSize, lat sql.NullFloat64
			rows.Scan(&id, &s.Calls, &s.Injected, &injSize, &lat)
			s.AvgInjection = int(injSize.Float64)
			s.AvgLatencyMS = int64(lat.Float64)
			stats.ByRule[id] = s
		}
		rows.Close()
	}

	// by tool
	rows, err = db.Query(`SELECT tool_name, COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? GROUP BY tool_name`, since)
	if err == nil {
		for rows.Next() {
			var t string
			var n int
			rows.Scan(&t, &n)
			stats.ByTool[t] = n
		}
		rows.Close()
	}

	// top paths
	rows, err = db.Query(`SELECT path, COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? AND path != '' GROUP BY path ORDER BY COUNT(*) DESC LIMIT 15`, since)
	if err == nil {
		for rows.Next() {
			var p toolHookPathStat
			rows.Scan(&p.Path, &p.Count)
			stats.TopPaths = append(stats.TopPaths, p)
		}
		rows.Close()
	}

	// skip breakdown
	rows, err = db.Query(`SELECT skip_reason, COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? AND skip_reason != '' GROUP BY skip_reason`, since)
	if err == nil {
		for rows.Next() {
			var r string
			var n int
			rows.Scan(&r, &n)
			stats.SkipBreakdown[r] = n
		}
		rows.Close()
	}

	return stats, nil
}

// handleToolHook dispatches the `tool-hook` subcommand.
func handleToolHook(args []string) {
	if len(args) == 0 {
		// no subcommand = run as hook (stdin → stdout injection)
		runToolHook()
		return
	}
	switch args[0] {
	case "stats":
		handleToolHookStats(args[1:])
	case "test":
		handleToolHookTest(args[1:])
	case "rules":
		handleToolHookRules()
	default:
		fmt.Fprintf(os.Stderr, "usage: %s tool-hook [stats|test|rules]\n", appName)
		fmt.Fprintf(os.Stderr, "  (no args = run as PreToolUse hook, reads JSON from stdin)\n")
		os.Exit(1)
	}
}

func handleToolHookStats(args []string) {
	days := 7
	for i, a := range args {
		if a == "--days" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &days)
		}
	}
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	stats, err := queryToolHookStats(db, days)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(stats)
}

func handleToolHookTest(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s tool-hook test <tool_name> <path>\n", appName)
		os.Exit(1)
	}
	tool, path := args[0], args[1]
	cfg, err := loadToolHookConfig(defaultToolHookConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	hit := 0
	for _, rule := range cfg.Rules {
		if rule.Disabled {
			continue
		}
		if !rule.toolMatches(tool) {
			continue
		}
		if !matchesGlob(path, rule.Match) {
			continue
		}
		hit++
		fmt.Printf("✓ match  %s  (priority=%d, budget=%d, dedupe=%s)\n",
			rule.ID, rule.Priority, rule.Budget, rule.Dedupe)
		fmt.Printf("         inject: %s\n", trimForDisplay(rule.Inject, 100))
	}
	if hit == 0 {
		fmt.Printf("no match for tool=%s path=%s\n", tool, path)
	}
}

func handleToolHookRules() {
	cfg, err := loadToolHookConfig(defaultToolHookConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("config: %s\n", defaultToolHookConfigPath())
	fmt.Printf("global budget: %d chars\n", cfg.Budget)
	fmt.Printf("%d rules:\n\n", len(cfg.Rules))
	for _, r := range cfg.Rules {
		status := "enabled"
		if r.Disabled {
			status = "DISABLED"
		}
		fmt.Printf("• %s [%s] priority=%d budget=%d dedupe=%s\n",
			r.ID, status, r.Priority, r.Budget, r.Dedupe)
		fmt.Printf("    tools: %v\n", r.Tools)
		fmt.Printf("    match: %v\n", r.Match)
		fmt.Printf("    inject: %s\n\n", trimForDisplay(r.Inject, 120))
	}
}

func trimForDisplay(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
