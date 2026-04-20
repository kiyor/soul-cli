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

// ── Hook translator: event-aware system-reminder injection ──
//
// Claude Code invokes this binary with a JSON payload on stdin for any hook
// event (PreToolUse, UserPromptSubmit, SessionStart, Stop, ...). We dispatch
// on hook_event_name and run the matching rules from YAML.
//
//   PreToolUse         → tool_name + file path glob (ToolHookRule.Tools / .Match)
//   UserPromptSubmit   → prompt regex (ToolHookRule.MatchPrompt)
//   (future events)    → added case by case
//
// Any stdout the hook produces is injected as system-reminder context.
// Exit 0 = allow, non-zero = block (we never block; injection is advisory).
//
// Every invocation writes one row to tool_hook_audit. The table doubles as
// dedup state (per_session / per_file): dedup checks query WHERE injected=1.

// Supported hook event names — mirrors Claude Code's settings.json-level
// allowlist in src/utils/settings/settings.ts (as of upstream 2026-04).
// All are audited; only the ones with bespoke matchers (PreToolUse,
// UserPromptSubmit) run rule matching. Everything else is audit-only.
const (
	HookEventPreToolUse       = "PreToolUse"
	HookEventPostToolUse      = "PostToolUse"
	HookEventNotification     = "Notification"
	HookEventUserPromptSubmit = "UserPromptSubmit"
	HookEventSessionStart     = "SessionStart"
	HookEventSessionEnd       = "SessionEnd"
	HookEventStop             = "Stop"
	HookEventSubagentStop     = "SubagentStop"
	HookEventPreCompact       = "PreCompact"
	HookEventPostCompact      = "PostCompact"
	HookEventTeammateIdle     = "TeammateIdle"
	HookEventTaskCreated      = "TaskCreated"
	HookEventTaskCompleted    = "TaskCompleted"
)

// AllHookEvents is the ordered list Claude Code accepts at the top level of
// settings.json's `hooks` object. Kept in sync with Claude Code upstream.
// Adding a new event here also requires adding it to settings.json for the
// binary to be invoked; otherwise no audit is written for it.
var AllHookEvents = []string{
	HookEventPreToolUse,
	HookEventPostToolUse,
	HookEventNotification,
	HookEventUserPromptSubmit,
	HookEventSessionStart,
	HookEventSessionEnd,
	HookEventStop,
	HookEventSubagentStop,
	HookEventPreCompact,
	HookEventPostCompact,
	HookEventTeammateIdle,
	HookEventTaskCreated,
	HookEventTaskCompleted,
}

// knownHookEvents is the set form of AllHookEvents for O(1) lookup.
// Events outside this set are still audited but flagged as "unknown event"
// so we notice when Claude Code adds a new hook we haven't wired.
var knownHookEvents = func() map[string]bool {
	m := make(map[string]bool, len(AllHookEvents))
	for _, e := range AllHookEvents {
		m[e] = true
	}
	return m
}()

// defaultEvent is assumed when a rule's Events list is empty — preserves
// backward compatibility with the original path-only tool-hook YAML.
const defaultEvent = HookEventPreToolUse

// ToolHookInput mirrors the payload Claude Code sends via stdin. Fields are a
// union across supported events; unused ones are zero.
type ToolHookInput struct {
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`       // PreToolUse
	ToolInput      json.RawMessage `json:"tool_input"`      // PreToolUse
	Prompt         string          `json:"prompt"`          // UserPromptSubmit
	HookEventName  string          `json:"hook_event_name"`
	TranscriptPath string          `json:"transcript_path"`
}

// ToolHookRule is one YAML rule definition.
type ToolHookRule struct {
	ID          string   `yaml:"id"`
	Events      []string `yaml:"events"`       // which hook events the rule applies to; empty = [PreToolUse]
	Match       []string `yaml:"match"`        // glob patterns against target path (PreToolUse)
	Tools       []string `yaml:"tools"`        // Read / Edit / Write / Grep / Glob; empty = any (PreToolUse)
	MatchPrompt []string `yaml:"match_prompt"` // regex patterns against user prompt (UserPromptSubmit)
	Inject      string   `yaml:"inject"`       // system-reminder body (trimmed)
	Dedupe      string   `yaml:"dedupe"`       // never | per_session | per_file (default per_file)
	Budget      int      `yaml:"budget"`       // per-rule max chars; 0 = 500 default
	Priority    int      `yaml:"priority"`     // higher fires first; 0 = 50 default
	Disabled    bool     `yaml:"disabled"`
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

// eventMatches returns true if the rule applies to the given hook event.
// Empty Events list defaults to [PreToolUse] — keeps legacy rules working
// without a YAML migration.
func (r *ToolHookRule) eventMatches(event string) bool {
	if len(r.Events) == 0 {
		return event == defaultEvent
	}
	for _, e := range r.Events {
		if strings.EqualFold(e, event) {
			return true
		}
	}
	return false
}

// matchesPromptRegex returns true if prompt matches any of the regex patterns.
// Patterns are compiled on first use and cached. Invalid patterns are skipped.
func matchesPromptRegex(prompt string, patterns []string) bool {
	if prompt == "" || len(patterns) == 0 {
		return false
	}
	for _, pat := range patterns {
		rx := compilePromptRegex(pat)
		if rx != nil && rx.MatchString(prompt) {
			return true
		}
	}
	return false
}

var promptRegexCache sync.Map // pattern → *regexp.Regexp (or nil sentinel on bad pattern)

type badRegex struct{}

var badRegexSentinel = &badRegex{}

func compilePromptRegex(pattern string) *regexp.Regexp {
	if v, ok := promptRegexCache.Load(pattern); ok {
		if rx, ok := v.(*regexp.Regexp); ok {
			return rx
		}
		return nil // sentinel = known-bad
	}
	rx, err := regexp.Compile(pattern)
	if err != nil {
		promptRegexCache.Store(pattern, badRegexSentinel)
		return nil
	}
	promptRegexCache.Store(pattern, rx)
	return rx
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
	if row.EventName == "" {
		row.EventName = defaultEvent
	}
	_, err := db.Exec(`INSERT INTO tool_hook_audit
		(timestamp, session_id, cwd, event_name, tool_name, path, rule_id, injected, skip_reason, injection_size, budget_used, latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.Timestamp, row.SessionID, row.CWD, row.EventName, row.ToolName, row.Path,
		row.RuleID, row.Injected, row.SkipReason, row.InjectionSize, row.BudgetUsed, row.LatencyMS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tool-hook] audit write failed: %v\n", err)
	}
}

type toolHookAuditRow struct {
	Timestamp     string
	SessionID     string
	CWD           string
	EventName     string
	ToolName      string
	Path          string
	RuleID        string
	Injected      bool
	SkipReason    string
	InjectionSize int
	BudgetUsed    int
	LatencyMS     int64
}

// runToolHook is the entry point when weiran is invoked as a Claude Code hook.
// Reads JSON from stdin, dispatches on hook_event_name, writes optional
// system-reminder JSON to stdout. Never errors — hook failures must not block.
func runToolHook() {
	if os.Getenv("WEIRAN_HOOK_TRACE") == "1" {
		traceStart := time.Now()
		defer func() {
			fmt.Fprintf(os.Stderr, "[trace] total=%s\n", time.Since(traceStart))
		}()
	}

	rawIn, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	if os.Getenv("WEIRAN_HOOK_TRACE") == "1" {
		fmt.Fprintf(os.Stderr, "[trace] stdin_read ok\n")
	}
	var in ToolHookInput
	if err := json.Unmarshal(rawIn, &in); err != nil {
		return
	}

	// Default legacy payloads (no hook_event_name) to PreToolUse so older
	// Claude Code versions keep working unchanged.
	event := in.HookEventName
	if event == "" {
		event = defaultEvent
	}

	cfg, err := loadToolHookConfig(defaultToolHookConfigPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tool-hook] config load error: %v\n", err)
		return
	}

	db, _ := openDB()
	if db != nil {
		defer db.Close()
	}

	switch event {
	case HookEventPreToolUse:
		runPreToolUseHook(in, cfg, db)
	case HookEventUserPromptSubmit:
		runUserPromptSubmitHook(in, cfg, db)
	default:
		// All other events (PostToolUse, Stop, SessionStart, Notification,
		// PreCompact, ...) are audit-only for now: one row per invocation, no
		// injection. Future bespoke matchers can be added by name.
		runAuditOnlyHook(event, in, db)
	}
}

// runAuditOnlyHook records a single observability row for events that don't
// (yet) have a rule matcher. The audit row still captures session_id / cwd /
// event_name / tool_name (if PostToolUse) so we can see what Claude Code does.
func runAuditOnlyHook(event string, in ToolHookInput, db *sql.DB) {
	if db == nil {
		return
	}
	start := time.Now()
	// For PostToolUse we keep the tool_name + path so tool usage is observable
	// end-to-end; for other events those stay empty.
	var toolName, path string
	if event == HookEventPostToolUse {
		toolName = in.ToolName
		path = extractToolPath(in.ToolName, in.ToolInput)
	}
	writeToolHookAudit(db, toolHookAuditRow{
		Timestamp: time.Now().Format(time.RFC3339),
		SessionID: in.SessionID,
		CWD:       in.CWD,
		EventName: event,
		ToolName:  toolName,
		Path:      path,
		LatencyMS: time.Since(start).Milliseconds(),
	})
}

// runPreToolUseHook handles the original path + tool glob matching.
func runPreToolUseHook(in ToolHookInput, cfg *ToolHookConfig, db *sql.DB) {
	start := time.Now()
	path := extractToolPath(in.ToolName, in.ToolInput)
	sessionID := in.SessionID

	audited := false
	budgetRemaining := cfg.Budget
	var injections []string
	budgetUsedTotal := 0

	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.ID == "" {
			continue
		}
		if !rule.eventMatches(HookEventPreToolUse) {
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
				EventName:  HookEventPreToolUse,
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
				EventName:  HookEventPreToolUse,
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
			EventName:     HookEventPreToolUse,
			ToolName:      in.ToolName,
			Path:          path,
			RuleID:        rule.ID,
			Injected:      true,
			InjectionSize: len(body),
			BudgetUsed:    budgetUsedTotal,
			LatencyMS:     time.Since(start).Milliseconds(),
		})
	}

	if !audited && db != nil {
		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp: time.Now().Format(time.RFC3339),
			SessionID: sessionID,
			CWD:       in.CWD,
			EventName: HookEventPreToolUse,
			ToolName:  in.ToolName,
			Path:      path,
			LatencyMS: time.Since(start).Milliseconds(),
		})
	}

	emitInjections(HookEventPreToolUse, injections)
}

// runUserPromptSubmitHook handles prompt regex matching for the user's message.
// Dedupe scope uses session_id only (no "path" concept for UserPromptSubmit);
// the audit table's path column is reused as a short prompt digest (first 120
// chars) for observability.
func runUserPromptSubmitHook(in ToolHookInput, cfg *ToolHookConfig, db *sql.DB) {
	start := time.Now()
	sessionID := in.SessionID
	digest := promptDigest(in.Prompt, 120)

	audited := false
	budgetRemaining := cfg.Budget
	var injections []string
	budgetUsedTotal := 0

	for _, rule := range cfg.Rules {
		if rule.Disabled || rule.ID == "" {
			continue
		}
		if !rule.eventMatches(HookEventUserPromptSubmit) {
			continue
		}
		if !matchesPromptRegex(in.Prompt, rule.MatchPrompt) {
			continue
		}
		audited = true

		// For UserPromptSubmit we dedupe per_session (and per_file is treated
		// as per_session too since there is no file path).
		if db != nil && isDedupedPrompt(db, rule, sessionID) {
			writeToolHookAudit(db, toolHookAuditRow{
				Timestamp:  time.Now().Format(time.RFC3339),
				SessionID:  sessionID,
				CWD:        in.CWD,
				EventName:  HookEventUserPromptSubmit,
				Path:       digest,
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
				EventName:  HookEventUserPromptSubmit,
				Path:       digest,
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
			EventName:     HookEventUserPromptSubmit,
			Path:          digest,
			RuleID:        rule.ID,
			Injected:      true,
			InjectionSize: len(body),
			BudgetUsed:    budgetUsedTotal,
			LatencyMS:     time.Since(start).Milliseconds(),
		})
	}

	if !audited && db != nil {
		writeToolHookAudit(db, toolHookAuditRow{
			Timestamp: time.Now().Format(time.RFC3339),
			SessionID: sessionID,
			CWD:       in.CWD,
			EventName: HookEventUserPromptSubmit,
			Path:      digest,
			LatencyMS: time.Since(start).Milliseconds(),
		})
	}

	emitInjections(HookEventUserPromptSubmit, injections)
}

// isDedupedPrompt is the UserPromptSubmit dedupe check (session-scoped only).
// "never" → always allow; anything else → 1 per (rule, session).
func isDedupedPrompt(db *sql.DB, rule ToolHookRule, sessionID string) bool {
	if rule.Dedupe == "never" {
		return false
	}
	var n int
	db.QueryRow(`SELECT 1 FROM tool_hook_audit
		WHERE rule_id=? AND session_id=? AND event_name=? AND injected=1 LIMIT 1`,
		rule.ID, sessionID, HookEventUserPromptSubmit).Scan(&n)
	return n == 1
}

// promptDigest returns a short excerpt of the prompt for audit-table observability.
// Kept tiny; the full prompt lives in Claude Code's own session JSONL.
func promptDigest(prompt string, n int) string {
	p := strings.ReplaceAll(strings.ReplaceAll(prompt, "\n", " "), "\r", " ")
	if len([]rune(p)) <= n {
		return p
	}
	runes := []rune(p)
	return string(runes[:n]) + "…"
}

// emitInjections writes the final JSON output that Claude Code consumes.
// Nothing is written when there are no injections.
func emitInjections(event string, injections []string) {
	if len(injections) == 0 {
		return
	}
	body := strings.Join(injections, "\n\n")
	out := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName":     event,
			"additionalContext": body,
		},
		"suppressOutput": true,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(out)
}

// ── CLI: `weiran tool-hook stats` ──

// toolHookStats aggregates recent audit rows for diagnostics.
type toolHookStats struct {
	TotalCalls      int                      `json:"total_calls"`
	MatchedCalls    int                      `json:"matched_calls"`
	InjectedCalls   int                      `json:"injected_calls"`
	ByRule          map[string]toolHookRuleS `json:"by_rule"`
	ByTool          map[string]int           `json:"by_tool"`
	ByEvent         map[string]int           `json:"by_event"`
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
		ByEvent:       map[string]int{},
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

	// by event
	rows, err = db.Query(`SELECT event_name, COUNT(*) FROM tool_hook_audit
		WHERE timestamp >= ? GROUP BY event_name`, since)
	if err == nil {
		for rows.Next() {
			var e string
			var n int
			rows.Scan(&e, &n)
			stats.ByEvent[e] = n
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
	case "events":
		handleToolHookEvents()
	case "gc":
		handleToolHookGC(args[1:])
	case "log":
		handleToolHookLog(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "usage: %s tool-hook <subcommand>\n\n", appName)
		fmt.Fprintf(os.Stderr, "  (no args)            run as hook (reads JSON from stdin)\n")
		fmt.Fprintf(os.Stderr, "  stats [--days N] [--json]\n                       aggregated audit stats (human by default)\n")
		fmt.Fprintf(os.Stderr, "  log   [filters…]     tail recent audit rows (see log --help)\n")
		fmt.Fprintf(os.Stderr, "  test  <tool> <path>  dry-run PreToolUse rule matching\n")
		fmt.Fprintf(os.Stderr, "  rules                list configured rules (YAML)\n")
		fmt.Fprintf(os.Stderr, "  events               list hook events this binary recognizes\n")
		fmt.Fprintf(os.Stderr, "  gc    [--days N]     delete audit rows older than N days (default 30)\n")
		os.Exit(1)
	}
}

// handleToolHookLog dumps recent audit rows with filtering. Output format is
// a compact single-line-per-row KV pairs which is greppable and fits terminal.
//
// Filters (combinable, AND semantics):
//   --event    <name>     PreToolUse / UserPromptSubmit / …
//   --session  <id>       exact session id match
//   --rule     <id>       only rows where this rule fired or skipped
//   --tool     <name>     only PreToolUse/PostToolUse rows for this tool
//   --injected             only rows that actually injected
//   --skipped              only rows with a skip_reason (dedupe/budget)
//   --grep     <substr>   substring match against path/prompt digest
//   --days     <N>        lookback window (default 1)
//   --limit    <N>        row cap (default 50, max 1000)
//   --json                emit NDJSON instead of human format
//   --help                usage
func handleToolHookLog(args []string) {
	days := 1
	limit := 50
	var filterEvent, filterSession, filterRule, filterTool, filterGrep string
	jsonOut := false
	onlyInjected := false
	onlySkipped := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		peek := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "--help", "-h":
			fmt.Print(`weiran tool-hook log — search the audit table.

Filters (all combinable):
  --event <name>     PreToolUse | PostToolUse | UserPromptSubmit | Stop | …
  --session <id>     exact session id
  --rule <id>        rule id (match or skip)
  --tool <name>      Read | Edit | Write | Bash | …
  --injected         only rows that actually injected context
  --skipped          only rows with a skip_reason
  --grep <substr>    substring match against path/prompt digest
  --days <N>         lookback window (default 1)
  --limit <N>        row cap (default 50, max 1000)
  --json             NDJSON output instead of human format

Examples:
  weiran tool-hook log --event UserPromptSubmit --days 7
  weiran tool-hook log --rule remember_signal --injected
  weiran tool-hook log --grep memory/topics/feedback --limit 100
  weiran tool-hook log --tool Edit --days 3 --json | jq .
`)
			return
		case "--event":
			filterEvent = peek()
		case "--session":
			filterSession = peek()
		case "--rule":
			filterRule = peek()
		case "--tool":
			filterTool = peek()
		case "--grep":
			filterGrep = peek()
		case "--days":
			fmt.Sscanf(peek(), "%d", &days)
		case "--limit":
			fmt.Sscanf(peek(), "%d", &limit)
		case "--json":
			jsonOut = true
		case "--injected":
			onlyInjected = true
		case "--skipped":
			onlySkipped = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s (try --help)\n", a)
			os.Exit(1)
		}
	}
	if days < 1 {
		days = 1
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Build parameterized query to keep grep noise minimal.
	since := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	q := `SELECT timestamp, session_id, event_name, tool_name, path, rule_id,
		injected, skip_reason, injection_size, budget_used, latency_ms
		FROM tool_hook_audit WHERE timestamp >= ?`
	params := []interface{}{since}
	if filterEvent != "" {
		q += " AND event_name=?"
		params = append(params, filterEvent)
	}
	if filterSession != "" {
		q += " AND session_id=?"
		params = append(params, filterSession)
	}
	if filterRule != "" {
		q += " AND rule_id=?"
		params = append(params, filterRule)
	}
	if filterTool != "" {
		q += " AND tool_name=?"
		params = append(params, filterTool)
	}
	if onlyInjected {
		q += " AND injected=1"
	}
	if onlySkipped {
		q += " AND skip_reason != ''"
	}
	if filterGrep != "" {
		q += " AND path LIKE ?"
		params = append(params, "%"+filterGrep+"%")
	}
	q += " ORDER BY id DESC LIMIT ?"
	params = append(params, limit)

	rows, err := db.Query(q, params...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	type logRow struct {
		Timestamp     string `json:"timestamp"`
		SessionID     string `json:"session_id"`
		EventName     string `json:"event_name"`
		ToolName      string `json:"tool_name"`
		Path          string `json:"path"`
		RuleID        string `json:"rule_id"`
		Injected      bool   `json:"injected"`
		SkipReason    string `json:"skip_reason"`
		InjectionSize int    `json:"injection_size"`
		BudgetUsed    int    `json:"budget_used"`
		LatencyMS     int64  `json:"latency_ms"`
	}
	var out []logRow
	for rows.Next() {
		var r logRow
		var inj int
		if err := rows.Scan(&r.Timestamp, &r.SessionID, &r.EventName, &r.ToolName,
			&r.Path, &r.RuleID, &inj, &r.SkipReason, &r.InjectionSize,
			&r.BudgetUsed, &r.LatencyMS); err != nil {
			continue
		}
		r.Injected = inj == 1
		out = append(out, r)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		for _, r := range out {
			enc.Encode(r)
		}
		return
	}

	if len(out) == 0 {
		fmt.Fprintf(os.Stderr, "no rows matched (try --days %d --limit %d or relax filters)\n", days, limit)
		return
	}

	// Human output: rows newest-first. Each line is a stable KV sequence so
	// `grep event=UserPromptSubmit` / `awk '$0 ~ /rule=remember/'` still work.
	for _, r := range out {
		verdict := "observe"
		if r.Injected {
			verdict = "INJECT"
		} else if r.SkipReason != "" {
			verdict = "skip:" + r.SkipReason
		}
		line := fmt.Sprintf("%s  event=%-17s tool=%-10s rule=%-30s verdict=%-15s sess=%s",
			r.Timestamp, r.EventName, nz(r.ToolName), nz(r.RuleID), verdict, shortSessionID(r.SessionID, 8))
		if r.Path != "" {
			line += "  path=" + r.Path
		}
		if r.Injected || r.InjectionSize > 0 {
			line += fmt.Sprintf("  bytes=%d", r.InjectionSize)
		}
		fmt.Println(line)
	}
	fmt.Fprintf(os.Stderr, "\n(%d rows in last %dd; --limit %d, --days %d)\n", len(out), days, limit, days)
}

// nz returns "-" when s is empty, keeping log columns aligned.
func nz(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortSessionID truncates a UUID-ish session id for visual density. Named
// explicitly to avoid colliding with the workspace-aware `shortID` in safe.go.
func shortSessionID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// handleToolHookEvents prints the list of hook events this binary recognizes.
// Useful when wiring up ~/.claude/settings.json: any event printed here can
// be safely registered; anything else will be audit-only with "unknown event".
func handleToolHookEvents() {
	fmt.Printf("%d hook events recognized (per Claude Code settings.json allowlist):\n\n", len(AllHookEvents))
	for _, e := range AllHookEvents {
		matcher := "audit-only"
		switch e {
		case HookEventPreToolUse:
			matcher = "rule-driven (tools + match glob)"
		case HookEventUserPromptSubmit:
			matcher = "rule-driven (match_prompt regex)"
		}
		fmt.Printf("  %-20s %s\n", e, matcher)
	}
	fmt.Println("\nEvents not listed above are still accepted — their event_name is recorded verbatim in the audit table, flagged as unknown.")
}

// handleToolHookGC deletes audit rows older than `days`. Defaults to 30 days.
// Safe to run concurrently (DELETE is atomic). Reports how many rows were
// removed and the resulting table row count.
func handleToolHookGC(args []string) {
	days := 30
	for i, a := range args {
		if a == "--days" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &days)
		}
	}
	if days < 1 {
		fmt.Fprintf(os.Stderr, "refusing to gc with --days < 1 (would delete everything)\n")
		os.Exit(1)
	}
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	cutoff := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	res, err := db.Exec(`DELETE FROM tool_hook_audit WHERE timestamp < ?`, cutoff)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gc failed: %v\n", err)
		os.Exit(1)
	}
	deleted, _ := res.RowsAffected()

	var remaining int
	db.QueryRow(`SELECT COUNT(*) FROM tool_hook_audit`).Scan(&remaining)

	// VACUUM is cheap after a big DELETE and keeps the sqlite file from
	// growing unbounded. Best-effort; ignore errors.
	_, _ = db.Exec(`VACUUM`)

	fmt.Printf("deleted %d rows older than %d days (cutoff=%s); %d rows remaining\n",
		deleted, days, cutoff, remaining)
}

func handleToolHookStats(args []string) {
	days := 7
	jsonOut := false
	for i, a := range args {
		switch a {
		case "--days":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &days)
			}
		case "--json":
			jsonOut = true
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
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(stats)
		return
	}
	printToolHookStatsHuman(stats)
}

// printToolHookStatsHuman is the default stats output — aligned columns
// optimized for reading and grep-ability. Key lines start with a stable
// prefix (`rule=`, `event=`, `tool=`, `path=`) so `grep` and `awk` work.
func printToolHookStatsHuman(s *toolHookStats) {
	fmt.Printf("══ tool-hook stats (last %d days) ══\n", s.DaysQueried)
	fmt.Printf("calls=%d  matched=%d  injected=%d  avg_latency_ms=%d\n\n",
		s.TotalCalls, s.MatchedCalls, s.InjectedCalls, s.AvgLatencyMS)

	if len(s.ByEvent) > 0 {
		fmt.Println("── by event ──")
		keys := sortedStringKeys(s.ByEvent)
		for _, k := range keys {
			fmt.Printf("event=%-20s calls=%d\n", k, s.ByEvent[k])
		}
		fmt.Println()
	}

	if len(s.ByRule) > 0 {
		fmt.Println("── by rule ──")
		// Sort by calls descending for visual ranking.
		type kv struct {
			k string
			v toolHookRuleS
		}
		list := make([]kv, 0, len(s.ByRule))
		for k, v := range s.ByRule {
			list = append(list, kv{k, v})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].v.Calls > list[j].v.Calls })
		for _, e := range list {
			fmt.Printf("rule=%-36s calls=%-5d injected=%-5d avg_bytes=%-5d avg_latency_ms=%d\n",
				e.k, e.v.Calls, e.v.Injected, e.v.AvgInjection, e.v.AvgLatencyMS)
		}
		fmt.Println()
	}

	if len(s.ByTool) > 0 {
		fmt.Println("── by tool (PreToolUse/PostToolUse) ──")
		keys := sortedStringKeys(s.ByTool)
		for _, k := range keys {
			label := k
			if label == "" {
				label = "<none>"
			}
			fmt.Printf("tool=%-14s calls=%d\n", label, s.ByTool[k])
		}
		fmt.Println()
	}

	if len(s.TopPaths) > 0 {
		fmt.Println("── top paths ──")
		for _, p := range s.TopPaths {
			fmt.Printf("path=%-80s calls=%d\n", trimForDisplay(p.Path, 80), p.Count)
		}
		fmt.Println()
	}

	if len(s.SkipBreakdown) > 0 {
		fmt.Println("── skip reasons ──")
		keys := sortedStringKeys(s.SkipBreakdown)
		for _, k := range keys {
			fmt.Printf("skip=%-10s calls=%d\n", k, s.SkipBreakdown[k])
		}
	}
}

func sortedStringKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
