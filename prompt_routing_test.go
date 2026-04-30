package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// withMockRoutingConfig writes a YAML to a temp file, points the router at it,
// resets the cache, and registers cleanup. Tests use this instead of touching
// the real workspace routing.yaml.
func withMockRoutingConfig(t *testing.T, yaml string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "routing.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0644); err != nil {
		t.Fatalf("write mock config: %v", err)
	}
	prev := routingCfgPath
	resetRoutingConfigForTest(p)
	t.Cleanup(func() { resetRoutingConfigForTest(prev) })
}

// withEmptyRoutingConfig points router at a non-existent path (= no config).
// Validates the no-config fallback path.
func withEmptyRoutingConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prev := routingCfgPath
	resetRoutingConfigForTest(filepath.Join(dir, "does-not-exist.yaml"))
	t.Cleanup(func() { resetRoutingConfigForTest(prev) })
}

func TestIsValidSoulMode(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"core", true},
		{"emotional", true},
		{"intimate", true},
		{"technical", true},
		{"ops", true},
		{"evolve", true},
		{"", false},
		{"random", false},
		{"Emotional", false}, // case-sensitive on purpose
	}
	for _, c := range cases {
		if got := IsValidSoulMode(c.s); got != c.want {
			t.Errorf("IsValidSoulMode(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestDetectSoulMode_ExplicitOverride(t *testing.T) {
	withEmptyRoutingConfig(t)
	// Explicit valid mode wins over everything else
	sig := RoutingSignals{
		CLIMode:   "cron", // would normally → ops
		Explicit:  "intimate",
		LaunchDir: "/some/work/tree",
	}
	if got := detectSoulMode(sig); got != SoulModeIntimate {
		t.Errorf("explicit override: got %s, want intimate", got)
	}
}

func TestDetectSoulMode_ExplicitInvalidIgnored(t *testing.T) {
	withEmptyRoutingConfig(t)
	// Invalid explicit mode is silently ignored (security: no inject random strings)
	sig := RoutingSignals{
		CLIMode:  "cron",
		Explicit: "godmode",
	}
	if got := detectSoulMode(sig); got != SoulModeOps {
		t.Errorf("invalid explicit: got %s, want ops (cron fallback)", got)
	}
}

func TestDetectSoulMode_CronToOps(t *testing.T) {
	withEmptyRoutingConfig(t)
	sig := RoutingSignals{CLIMode: "cron"}
	if got := detectSoulMode(sig); got != SoulModeOps {
		t.Errorf("cron → ops failed, got %s", got)
	}
}

func TestDetectSoulMode_HeartbeatToOps(t *testing.T) {
	withEmptyRoutingConfig(t)
	sig := RoutingSignals{CLIMode: "heartbeat"}
	if got := detectSoulMode(sig); got != SoulModeOps {
		t.Errorf("heartbeat → ops failed, got %s", got)
	}
}

func TestDetectSoulMode_EvolveToEvolve(t *testing.T) {
	withEmptyRoutingConfig(t)
	sig := RoutingSignals{CLIMode: "evolve"}
	if got := detectSoulMode(sig); got != SoulModeEvolve {
		t.Errorf("evolve → evolve failed, got %s", got)
	}
}

func TestDetectSoulMode_IntimateTriggerFromConfig(t *testing.T) {
	withMockRoutingConfig(t, `
intimate_triggers:
  - '^/selfie\b'
  - '^TESTPHRASE'
`)
	cases := []struct {
		msg     string
		want    SoulMode
		comment string
	}{
		{"/selfie hello", SoulModeIntimate, "ASCII slash command"},
		{"TESTPHRASE blah", SoulModeIntimate, "configured phrase"},
		{"unrelated chatter", SoulModeEmotional, "no trigger → fallback"},
	}
	for _, c := range cases {
		sig := RoutingSignals{
			CLIMode:      "interactive",
			FirstMessage: c.msg,
		}
		if got := detectSoulMode(sig); got != c.want {
			t.Errorf("%s (msg=%q): got %s, want %s", c.comment, c.msg, got, c.want)
		}
	}
}

func TestDetectSoulMode_NoConfigIntimateNotTriggered(t *testing.T) {
	// Without a config, NO message can trigger intimate — even ones that
	// would have matched a hardcoded list. This proves framework code is
	// free of user-private trigger words.
	withEmptyRoutingConfig(t)
	sig := RoutingSignals{
		CLIMode:      "interactive",
		FirstMessage: "/selfie test", // no config = no triggers
	}
	if got := detectSoulMode(sig); got != SoulModeEmotional {
		t.Errorf("no config: got %s, want emotional fallback", got)
	}
}

func TestDetectSoulMode_TechnicalCwdFromConfig(t *testing.T) {
	withMockRoutingConfig(t, `
technical_cwd_prefixes:
  - /test/work-tree-A
  - /test/work-tree-B
`)
	cases := []struct {
		dir  string
		want SoulMode
	}{
		{"/test/work-tree-A/sub", SoulModeTechnical},
		{"/test/work-tree-B", SoulModeTechnical},
		{"/test/unrelated", SoulModeEmotional},
		{"", SoulModeEmotional},
	}
	for _, c := range cases {
		sig := RoutingSignals{
			CLIMode:   "interactive",
			LaunchDir: c.dir,
		}
		if got := detectSoulMode(sig); got != c.want {
			t.Errorf("dir=%q: got %s, want %s", c.dir, got, c.want)
		}
	}
}

func TestDetectSoulMode_HomeExpansion(t *testing.T) {
	// ~/ in config should expand to $HOME
	t.Setenv("HOME", "/test/home")
	withMockRoutingConfig(t, `
technical_cwd_prefixes:
  - '~/code'
  - '~/projects/work'
`)
	sig := RoutingSignals{
		CLIMode:   "interactive",
		LaunchDir: "/test/home/code/foo",
	}
	if got := detectSoulMode(sig); got != SoulModeTechnical {
		t.Errorf("~/ expansion: got %s, want technical", got)
	}
}

func TestDetectSoulMode_SourceDefaultFromConfig(t *testing.T) {
	withMockRoutingConfig(t, `
source_defaults:
  webui: emotional
  cli-batch: technical
`)
	cases := []struct {
		source string
		want   SoulMode
	}{
		{"webui", SoulModeEmotional},
		{"cli-batch", SoulModeTechnical},
		{"unknown", SoulModeEmotional}, // not in config → fallback
	}
	for _, c := range cases {
		sig := RoutingSignals{
			CLIMode: "interactive",
			Source:  c.source,
		}
		if got := detectSoulMode(sig); got != c.want {
			t.Errorf("source=%q: got %s, want %s", c.source, got, c.want)
		}
	}
}

func TestDetectSoulMode_ConfigurableFallback(t *testing.T) {
	withMockRoutingConfig(t, `
fallback: technical
`)
	sig := RoutingSignals{CLIMode: "interactive"}
	if got := detectSoulMode(sig); got != SoulModeTechnical {
		t.Errorf("fallback: got %s, want technical", got)
	}
}

func TestDetectSoulMode_InvalidFallbackIgnored(t *testing.T) {
	withMockRoutingConfig(t, `
fallback: bogus
`)
	sig := RoutingSignals{CLIMode: "interactive"}
	if got := detectSoulMode(sig); got != SoulModeEmotional {
		t.Errorf("invalid fallback: got %s, want emotional default", got)
	}
}

func TestDetectSoulMode_FallbackToEmotional(t *testing.T) {
	withEmptyRoutingConfig(t)
	sig := RoutingSignals{
		CLIMode: "interactive",
	}
	if got := detectSoulMode(sig); got != SoulModeEmotional {
		t.Errorf("fallback: got %s, want emotional", got)
	}
}

func TestDetectSoulMode_Precedence_TechnicalCwdBeatsSourceDefault(t *testing.T) {
	// cwd signal precedes source signal in the precedence chain.
	withMockRoutingConfig(t, `
technical_cwd_prefixes:
  - /test/work
source_defaults:
  webui: emotional
`)
	sig := RoutingSignals{
		CLIMode:   "interactive",
		Source:    "webui",
		LaunchDir: "/test/work/sub",
	}
	if got := detectSoulMode(sig); got != SoulModeTechnical {
		t.Errorf("cwd should beat source: got %s", got)
	}
}

func TestDetectSoulMode_Precedence_IntimateBeatsTechnicalCwd(t *testing.T) {
	// Intimate trigger from first message overrides technical cwd.
	withMockRoutingConfig(t, `
intimate_triggers:
  - '^TESTPHRASE'
technical_cwd_prefixes:
  - /test/work
`)
	sig := RoutingSignals{
		CLIMode:      "interactive",
		LaunchDir:    "/test/work/sub",
		FirstMessage: "TESTPHRASE hello",
	}
	if got := detectSoulMode(sig); got != SoulModeIntimate {
		t.Errorf("intimate trigger should beat technical cwd: got %s", got)
	}
}

func TestDetectSoulMode_MalformedConfigFallsBackSafely(t *testing.T) {
	// A YAML parse error must not crash the router; falls back to emotional.
	withMockRoutingConfig(t, "this is not valid: yaml: at all: [")
	sig := RoutingSignals{CLIMode: "interactive"}
	if got := detectSoulMode(sig); got != SoulModeEmotional {
		t.Errorf("malformed YAML: got %s, want emotional", got)
	}
}

func TestDetectSoulMode_InvalidRegexInConfigSkipped(t *testing.T) {
	// One bad pattern shouldn't disable the others.
	withMockRoutingConfig(t, `
intimate_triggers:
  - '['          # malformed
  - '^GOOD'      # valid
`)
	sig := RoutingSignals{
		CLIMode:      "interactive",
		FirstMessage: "GOOD trigger",
	}
	if got := detectSoulMode(sig); got != SoulModeIntimate {
		t.Errorf("good pattern after bad one: got %s, want intimate", got)
	}
}

func TestRecordRoutingAudit_RoundTrip(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE prompt_routing_audit (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp       TEXT NOT NULL,
		cli_mode        TEXT NOT NULL DEFAULT '',
		soul_mode       TEXT NOT NULL DEFAULT '',
		signals_json    TEXT NOT NULL DEFAULT '{}',
		fragments_json  TEXT NOT NULL DEFAULT '[]',
		tokens_est      INTEGER NOT NULL DEFAULT 0,
		char_size       INTEGER NOT NULL DEFAULT 0,
		session_id      TEXT NOT NULL DEFAULT '',
		launch_dir      TEXT NOT NULL DEFAULT '',
		source          TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	d := PromptRoutingDecision{
		Timestamp: time.Date(2026, 4, 30, 15, 30, 0, 0, time.UTC),
		CLIMode:   "interactive",
		SoulMode:  SoulModeEmotional,
		Signals: RoutingSignals{
			CLIMode:      "interactive",
			LaunchDir:    "/some/dir",
			Source:       "webui",
			FirstMessage: "hello",
			Explicit:     "",
		},
		FragmentsUsed: []string{"core/01-identity.md", "persona/10-who-i-am.md"},
		TokensEst:     1500,
		CharSize:      6000,
		SessionID:     "abc-123",
	}
	writeRoutingAudit(db, d)

	var (
		cliMode, soulMode, signalsJSON, fragsJSON string
		tokens, chars                             int
		sessionID, launchDir, source              string
	)
	row := db.QueryRow(`SELECT cli_mode, soul_mode, signals_json, fragments_json,
	                            tokens_est, char_size, session_id, launch_dir, source
	                     FROM prompt_routing_audit ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&cliMode, &soulMode, &signalsJSON, &fragsJSON,
		&tokens, &chars, &sessionID, &launchDir, &source); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if cliMode != "interactive" || soulMode != "emotional" {
		t.Errorf("modes mismatch: cli=%s soul=%s", cliMode, soulMode)
	}
	if tokens != 1500 || chars != 6000 {
		t.Errorf("size mismatch: tokens=%d chars=%d", tokens, chars)
	}
	if sessionID != "abc-123" {
		t.Errorf("session_id = %q, want abc-123", sessionID)
	}
	if launchDir != "/some/dir" {
		t.Errorf("launch_dir = %q", launchDir)
	}
	if source != "webui" {
		t.Errorf("source = %q", source)
	}

	var sigs RoutingSignals
	if err := json.Unmarshal([]byte(signalsJSON), &sigs); err != nil {
		t.Fatalf("signals JSON unmarshal: %v", err)
	}
	if sigs.FirstMessage != "hello" {
		t.Errorf("signals.FirstMessage = %q", sigs.FirstMessage)
	}

	var frags []string
	if err := json.Unmarshal([]byte(fragsJSON), &frags); err != nil {
		t.Fatalf("frags JSON unmarshal: %v", err)
	}
	if len(frags) != 2 || frags[0] != "core/01-identity.md" {
		t.Errorf("fragments mismatch: %v", frags)
	}
}

func TestFragmentFrontmatterSpec_RequiredFields(t *testing.T) {
	required := map[string]bool{}
	for _, f := range fragmentFrontmatterSpec.RequiredFields {
		required[f] = true
	}
	for _, must := range []string{"id", "title", "modes"} {
		if !required[must] {
			t.Errorf("frontmatter spec missing required field: %s", must)
		}
	}
}

// ── Phase B: Fragment validator / loader tests ────────────────────────────────

// writeFragment creates a fragment file with the given frontmatter and body in dir.
func writeFragment(t *testing.T, dir, name, frontmatter, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	content := "---\n" + frontmatter + "\n---\n\n" + body
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("writeFragment %s: %v", name, err)
	}
	return p
}

// withSoulDir sets soulFragmentsDirOverride and resets it after the test.
func withSoulDir(t *testing.T, dir string) {
	t.Helper()
	prev := soulFragmentsDirOverride
	soulFragmentsDirOverride = dir
	t.Cleanup(func() { soulFragmentsDirOverride = prev })
}

func TestValidateSoulFragments_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	withSoulDir(t, dir)
	// Empty directory → no warnings
	if w := validateSoulFragments(); len(w) != 0 {
		t.Errorf("empty dir: want 0 warnings, got %v", w)
	}
}

func TestValidateSoulFragments_ValidFragment(t *testing.T) {
	dir := t.TempDir()
	withSoulDir(t, dir)
	writeFragment(t, dir, "01-identity.md",
		`id: 01-identity
title: Identity
modes: [core, emotional, intimate, evolve]`,
		"# Identity\n\nContent here.\n")
	if w := validateSoulFragments(); len(w) != 0 {
		t.Errorf("valid fragment: want 0 warnings, got %v", w)
	}
}

func TestValidateSoulFragments_MissingFrontmatter(t *testing.T) {
	dir := t.TempDir()
	withSoulDir(t, dir)
	// Write a plain markdown file without frontmatter
	if err := os.WriteFile(filepath.Join(dir, "01-identity.md"), []byte("# no frontmatter\n"), 0644); err != nil {
		t.Fatal(err)
	}
	w := validateSoulFragments()
	if len(w) == 0 {
		t.Error("missing frontmatter: expected a warning")
	}
}

func TestValidateSoulFragments_MissingRequiredField(t *testing.T) {
	dir := t.TempDir()
	withSoulDir(t, dir)
	// Missing 'title'
	writeFragment(t, dir, "01-identity.md",
		`id: 01-identity
modes: [core, emotional]`,
		"content\n")
	w := validateSoulFragments()
	found := false
	for _, warning := range w {
		if strings.Contains(warning, "title") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing title field should warn; got: %v", w)
	}
}

func TestValidateSoulFragments_IDMismatch(t *testing.T) {
	dir := t.TempDir()
	withSoulDir(t, dir)
	writeFragment(t, dir, "01-identity.md",
		`id: wrong-id
title: Identity
modes: [core]`,
		"content\n")
	w := validateSoulFragments()
	found := false
	for _, warning := range w {
		if strings.Contains(warning, "id mismatch") {
			found = true
		}
	}
	if !found {
		t.Errorf("id mismatch should warn; got: %v", w)
	}
}

func TestValidateSoulFragments_InvalidMode(t *testing.T) {
	dir := t.TempDir()
	withSoulDir(t, dir)
	writeFragment(t, dir, "01-identity.md",
		`id: 01-identity
title: Identity
modes: [core, badmode]`,
		"content\n")
	w := validateSoulFragments()
	found := false
	for _, warning := range w {
		if strings.Contains(warning, "badmode") {
			found = true
		}
	}
	if !found {
		t.Errorf("invalid mode 'badmode' should warn; got: %v", w)
	}
}

func TestValidateSoulFragments_Subdirectory(t *testing.T) {
	dir := t.TempDir()
	withSoulDir(t, dir)
	// Create a fragment in a subdirectory (core/)
	subDir := filepath.Join(dir, "core")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFragment(t, subDir, "01-identity.md",
		`id: 01-identity
title: Identity
modes: [core, ops, emotional, intimate, evolve]`,
		"content\n")
	if w := validateSoulFragments(); len(w) != 0 {
		t.Errorf("valid fragment in subdir: want 0 warnings, got %v", w)
	}
}

// ── Fragment loader tests ─────────────────────────────────────────────────────

func TestLoadFragmentsByMode_FilterByMode(t *testing.T) {
	dir := t.TempDir()
	// Create fragments with different modes
	writeFragment(t, dir, "01-identity.md",
		`id: 01-identity
title: Identity
modes: [core, emotional, intimate, evolve]`,
		"# Identity\n")
	writeFragment(t, dir, "50-body.md",
		`id: 50-body
title: Body
modes: [intimate, evolve]`,
		"# Body\n")

	// emotional should only get 01-identity
	paths, err := loadFragmentsByMode(dir, SoulModeEmotional)
	if err != nil {
		t.Fatalf("loadFragmentsByMode: %v", err)
	}
	if len(paths) != 1 {
		t.Errorf("emotional: want 1 fragment, got %d: %v", len(paths), paths)
	}

	// intimate should get both
	paths, err = loadFragmentsByMode(dir, SoulModeIntimate)
	if err != nil {
		t.Fatalf("loadFragmentsByMode: %v", err)
	}
	if len(paths) != 2 {
		t.Errorf("intimate: want 2 fragments, got %d: %v", len(paths), paths)
	}
}

func TestLoadFragmentsByMode_NumericOrdering(t *testing.T) {
	dir := t.TempDir()
	// Write fragments with mixed numeric prefixes
	writeFragment(t, dir, "40-master.md",
		`id: 40-master
title: Master
modes: [emotional, intimate, evolve]`,
		"# Master\n")
	writeFragment(t, dir, "01-identity.md",
		`id: 01-identity
title: Identity
modes: [core, emotional, intimate, evolve]`,
		"# Identity\n")
	writeFragment(t, dir, "10-who.md",
		`id: 10-who
title: Who
modes: [emotional, intimate, evolve]`,
		"# Who\n")

	paths, err := loadFragmentsByMode(dir, SoulModeEmotional)
	if err != nil {
		t.Fatalf("loadFragmentsByMode: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("want 3 fragments, got %d", len(paths))
	}
	// Must be in order: 01 < 10 < 40
	keys := []int{
		extractFragmentSortKey(paths[0]),
		extractFragmentSortKey(paths[1]),
		extractFragmentSortKey(paths[2]),
	}
	if keys[0] != 1 || keys[1] != 10 || keys[2] != 40 {
		t.Errorf("wrong order: sort keys %v from paths %v", keys, paths)
	}
}

func TestLoadFragmentsByMode_EmotionalIsIntimatePrefix(t *testing.T) {
	// emotional fragment list must be a strict prefix of intimate fragment list —
	// this is the cache-friendly invariant.
	dir := t.TempDir()

	// Core fragments (all modes)
	writeFragment(t, dir, "01-identity.md",
		`id: 01-identity
title: Identity
modes: [core, ops, technical, emotional, intimate, evolve]`,
		"# Identity\n")
	writeFragment(t, dir, "02-master-essential.md",
		`id: 02-master-essential
title: Master Essential
modes: [core, ops, technical, emotional, intimate, evolve]`,
		"# Master\n")

	// Persona (emotional and above)
	writeFragment(t, dir, "10-who-i-am.md",
		`id: 10-who-i-am
title: Who I Am
modes: [emotional, intimate, evolve]`,
		"# Who\n")
	writeFragment(t, dir, "40-master-full.md",
		`id: 40-master-full
title: Master Full
modes: [emotional, intimate, evolve]`,
		"# Master Full\n")

	// Body (intimate and above)
	writeFragment(t, dir, "50-body.md",
		`id: 50-body
title: Body
modes: [intimate, evolve]`,
		"# Body\n")

	emotional, err := loadFragmentsByMode(dir, SoulModeEmotional)
	if err != nil {
		t.Fatalf("emotional: %v", err)
	}
	intimate, err := loadFragmentsByMode(dir, SoulModeIntimate)
	if err != nil {
		t.Fatalf("intimate: %v", err)
	}

	// emotional must be a prefix of intimate
	if len(emotional) >= len(intimate) {
		t.Fatalf("emotional(%d) must be shorter than intimate(%d)", len(emotional), len(intimate))
	}
	for i, p := range emotional {
		if intimate[i] != p {
			t.Errorf("prefix mismatch at index %d: emotional=%s, intimate=%s", i, p, intimate[i])
		}
	}
}

func TestAssembleFragments_StripsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	p := writeFragment(t, dir, "01-identity.md",
		`id: 01-identity
title: Identity
modes: [core]`,
		"# Identity\n\nSome content here.\n")

	result, err := assembleFragments([]string{p})
	if err != nil {
		t.Fatalf("assembleFragments: %v", err)
	}
	if strings.Contains(result, "---") {
		t.Errorf("assembled content should not contain frontmatter delimiters, got:\n%s", result)
	}
	if !strings.Contains(result, "# Identity") {
		t.Errorf("assembled content should contain body, got:\n%s", result)
	}
}

func TestAssembleFragments_MultipleFragments(t *testing.T) {
	dir := t.TempDir()
	p1 := writeFragment(t, dir, "01-identity.md",
		`id: 01-identity
title: Identity
modes: [core]`,
		"# Part One\n")
	p2 := writeFragment(t, dir, "02-master.md",
		`id: 02-master
title: Master
modes: [core]`,
		"# Part Two\n")

	result, err := assembleFragments([]string{p1, p2})
	if err != nil {
		t.Fatalf("assembleFragments: %v", err)
	}
	if !strings.Contains(result, "# Part One") || !strings.Contains(result, "# Part Two") {
		t.Errorf("both parts should be present, got:\n%s", result)
	}
	idx1 := strings.Index(result, "# Part One")
	idx2 := strings.Index(result, "# Part Two")
	if idx1 >= idx2 {
		t.Errorf("part one should precede part two")
	}
}

func TestParseModesList(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"[core, emotional, intimate]", []string{"core", "emotional", "intimate"}},
		{"[intimate]", []string{"intimate"}},
		{"core, ops", []string{"core", "ops"}},
		{"", nil},
		{"[]", nil},
	}
	for _, c := range cases {
		got := parseModesList(c.raw)
		if len(got) != len(c.want) {
			t.Errorf("parseModesList(%q): got %v, want %v", c.raw, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("parseModesList(%q)[%d]: got %q, want %q", c.raw, i, got[i], c.want[i])
			}
		}
	}
}

func TestExtractFragmentSortKey(t *testing.T) {
	cases := []struct {
		path string
		want int
	}{
		{"/soul/core/01-identity.md", 1},
		{"/soul/body/50-appearance.md", 50},
		{"/soul/master/40-master.md", 40},
		{"/soul/persona/no-number.md", 9999},
	}
	for _, c := range cases {
		if got := extractFragmentSortKey(c.path); got != c.want {
			t.Errorf("extractFragmentSortKey(%q) = %d, want %d", c.path, got, c.want)
		}
	}
}

// ── Phase C-1: assembly reorder tests ────────────────────────────────────────
//
// Goal: under flag-on, the SOUL section is loaded LAST in the static portion
// (just before the dynamic boundary), so the longest common prefix between
// two modes covers all non-mode-specific sections plus whatever SOUL fragments
// the modes share. Under flag-off the layout is unchanged (SOUL.md still
// inline with the other soul files).
//
// These tests exercise buildPrompt() against the real workspace, like the
// other TestBuildPrompt_* tests in prompt_test.go. They only read files —
// no writes — so they're safe to run against the live workspace.

// staticPortion returns the prompt up to and excluding the dynamic boundary
// marker. Dynamic content (current time, daily notes, TG context, etc.) is
// session-state-dependent and can't be byte-compared across calls.
func staticPortion(s string) string {
	const marker = "\n# ── 以下为动态内容"
	if i := strings.Index(s, marker); i >= 0 {
		return s[:i]
	}
	return s
}

// withFragmentLoading toggles enableFragmentLoading for one test and restores it.
func withFragmentLoading(t *testing.T, on bool) {
	t.Helper()
	prev := enableFragmentLoading
	enableFragmentLoading = on
	t.Cleanup(func() { enableFragmentLoading = prev })
}

// withSoulModeEnv sets WEIRAN_SOUL_MODE for one test (auto-restored by t.Setenv).
func withSoulModeEnv(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("WEIRAN_SOUL_MODE", mode)
}

// soulSectionStart returns the byte index of the SOUL.md section header,
// or -1 if absent.
func soulSectionStart(s string) int {
	return strings.Index(s, "\n# === SOUL.md ===\n")
}

func identitySectionStart(s string) int {
	return strings.Index(s, "\n# === IDENTITY.md ===\n")
}

func feedbackSectionStart(s string) int {
	return strings.Index(s, "\n# === Feedback (behavioral rules) ===\n")
}

// TestBuildPrompt_FlagOffLayoutUnchanged confirms that with flag=false the
// SOUL.md section comes BEFORE IDENTITY.md (legacy order, unchanged from
// pre-Phase-C). This is the structural counterpart to the byte-diff check
// captured manually before/after the reorder commit.
func TestBuildPrompt_FlagOffLayoutUnchanged(t *testing.T) {
	withFragmentLoading(t, false)
	result := buildPrompt()
	soul := soulSectionStart(result.content)
	ident := identitySectionStart(result.content)
	if soul < 0 {
		t.Fatal("flag-off prompt should contain SOUL.md section")
	}
	if ident < 0 {
		t.Fatal("flag-off prompt should contain IDENTITY.md section")
	}
	if soul >= ident {
		t.Errorf("flag-off: SOUL.md should appear BEFORE IDENTITY.md (legacy order); got soul=%d ident=%d", soul, ident)
	}
}

// TestBuildPrompt_FlagOnLayoutSoulIsLast confirms that with flag=true the
// SOUL.md section appears AFTER IDENTITY.md and AFTER the FEEDBACK section,
// just before the dynamic boundary. This is the C-1 reorder.
func TestBuildPrompt_FlagOnLayoutSoulIsLast(t *testing.T) {
	withFragmentLoading(t, true)
	withSoulModeEnv(t, "emotional")
	result := buildPrompt()
	soul := soulSectionStart(result.content)
	ident := identitySectionStart(result.content)
	feedback := feedbackSectionStart(result.content)
	if soul < 0 {
		t.Fatal("flag-on prompt should still emit a SOUL.md section header (now sourced from fragments)")
	}
	if ident < 0 {
		t.Fatal("flag-on prompt should contain IDENTITY.md section")
	}
	if soul <= ident {
		t.Errorf("flag-on: SOUL.md should appear AFTER IDENTITY.md (reorder); got soul=%d ident=%d", soul, ident)
	}
	if feedback >= 0 && soul <= feedback {
		t.Errorf("flag-on: SOUL.md should appear AFTER FEEDBACK; got soul=%d feedback=%d", soul, feedback)
	}
	// SOUL must still be in the static portion (before dynamic boundary).
	staticEnd := strings.Index(result.content, "\n# ── 以下为动态内容")
	if staticEnd < 0 {
		t.Fatal("dynamic boundary marker missing")
	}
	if soul >= staticEnd {
		t.Errorf("flag-on: SOUL.md leaked into dynamic portion; got soul=%d boundary=%d", soul, staticEnd)
	}
}

// runStaticPrompt builds a prompt under given flag/mode and returns the static
// portion. Restores all globals via t.Cleanup.
func runStaticPrompt(t *testing.T, flagOn bool, soulMode string) string {
	t.Helper()
	withFragmentLoading(t, flagOn)
	if soulMode != "" {
		withSoulModeEnv(t, soulMode)
	}
	r := buildPrompt()
	return staticPortion(r.content)
}

// TestBuildPrompt_PrefixPropertyEmotionalSubsetIntimate is the headline C-1
// invariant: an `emotional` prompt's static portion is a true byte prefix of
// an `intimate` prompt's static portion. This is what makes Anthropic API
// prompt cache reuse work across the emotional→intimate transition.
func TestBuildPrompt_PrefixPropertyEmotionalSubsetIntimate(t *testing.T) {
	em := runStaticPrompt(t, true, "emotional")
	in := runStaticPrompt(t, true, "intimate")
	if len(em) >= len(in) {
		t.Fatalf("emotional(%d) should be shorter than intimate(%d)", len(em), len(in))
	}
	if !strings.HasPrefix(in, em) {
		// Find first divergence for diagnostic.
		minLen := len(em)
		for i := 0; i < minLen; i++ {
			if em[i] != in[i] {
				ctxLo := i - 80
				if ctxLo < 0 {
					ctxLo = 0
				}
				ctxHi := i + 80
				if ctxHi > minLen {
					ctxHi = minLen
				}
				t.Fatalf("emotional is NOT byte prefix of intimate; first diff at byte %d:\n  emotional[%d:%d] = %q\n  intimate[%d:%d]  = %q",
					i, ctxLo, ctxHi, em[ctxLo:ctxHi], ctxLo, ctxHi, in[ctxLo:ctxHi])
			}
		}
		t.Fatal("emotional is not a strict prefix of intimate, but bytes match up to len(em)")
	}
}

// TestBuildPrompt_PrefixPropertyChain verifies the full nesting chain inside
// the same CLI mode (interactive): core ⊂ technical, core ⊂ emotional ⊂
// intimate, core ⊂ evolve. Each pair: shorter must be a true byte prefix of
// longer.
//
// Note: ops shares the same fragment set as core under the current spec, so
// they're equal (not strict subset). Tested separately below.
func TestBuildPrompt_PrefixPropertyChain(t *testing.T) {
	pairs := []struct {
		smaller string
		larger  string
	}{
		{"core", "technical"},
		{"core", "emotional"},
		{"emotional", "intimate"},
		{"core", "evolve"},
	}
	for _, p := range pairs {
		t.Run(p.smaller+"_subset_"+p.larger, func(t *testing.T) {
			small := runStaticPrompt(t, true, p.smaller)
			large := runStaticPrompt(t, true, p.larger)
			if len(small) >= len(large) {
				t.Fatalf("%s(%d) should be shorter than %s(%d)", p.smaller, len(small), p.larger, len(large))
			}
			if !strings.HasPrefix(large, small) {
				t.Fatalf("%s is NOT byte prefix of %s", p.smaller, p.larger)
			}
		})
	}
}

// TestBuildPrompt_CoreEqualsOps confirms ops and core load identical fragment
// sets (per the Phase B mode/fragment spec). Static portions should match
// byte-for-byte.
func TestBuildPrompt_CoreEqualsOps(t *testing.T) {
	core := runStaticPrompt(t, true, "core")
	ops := runStaticPrompt(t, true, "ops")
	if core != ops {
		t.Errorf("core and ops should produce identical static prompts (lengths: core=%d ops=%d)", len(core), len(ops))
	}
}

// TestBuildPrompt_IntimateEqualsEvolve confirms intimate and evolve load
// identical fragment sets in Phase B/C (evolve == all fragments == intimate).
func TestBuildPrompt_IntimateEqualsEvolve(t *testing.T) {
	intimate := runStaticPrompt(t, true, "intimate")
	evolve := runStaticPrompt(t, true, "evolve")
	if intimate != evolve {
		t.Errorf("intimate and evolve should produce identical static prompts (lengths: intimate=%d evolve=%d)", len(intimate), len(evolve))
	}
}

// TestBuildPrompt_BootCoreCommonPrefix verifies the cache-anchor invariant:
// every soul-mode shares the same BOOT+CORE prefix at the head of the prompt.
// This is the minimum guaranteed cache hit even when modes diverge.
func TestBuildPrompt_BootCoreCommonPrefix(t *testing.T) {
	modes := []string{"core", "ops", "technical", "emotional", "intimate", "evolve"}
	outputs := make([]string, len(modes))
	for i, m := range modes {
		outputs[i] = runStaticPrompt(t, true, m)
	}
	// CORE.md section header is the natural common-prefix anchor.
	const anchor = "# === CORE.md (read-only, do not modify) ==="
	for i, out := range outputs {
		idx := strings.Index(out, anchor)
		if idx < 0 {
			t.Fatalf("mode %s missing CORE.md section", modes[i])
		}
		if i > 0 && outputs[i][:idx] != outputs[0][:idx] {
			t.Errorf("BOOT prefix differs between mode %s and %s", modes[0], modes[i])
		}
	}
	// All modes must agree byte-for-byte through the end of CORE.md section.
	// Find end of CORE.md across all modes (the next "\n# === " after CORE).
	core0 := strings.Index(outputs[0], anchor)
	rest := outputs[0][core0+len(anchor):]
	nextSec := strings.Index(rest, "\n# === ")
	if nextSec < 0 {
		t.Fatal("can't find post-CORE section header")
	}
	commonEnd := core0 + len(anchor) + nextSec
	for i := 1; i < len(modes); i++ {
		if outputs[i][:commonEnd] != outputs[0][:commonEnd] {
			t.Errorf("BOOT+CORE prefix (%d bytes) differs between %s and %s", commonEnd, modes[0], modes[i])
		}
	}
}
