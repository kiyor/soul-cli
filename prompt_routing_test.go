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
