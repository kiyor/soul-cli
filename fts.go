package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FTS5 full-text search over daily notes and session summaries.
//
// Two external-content FTS5 virtual tables are maintained:
//
//   1. daily_notes_fts — indexes markdown files in workspace/memory/*.md
//      (daily diary entries, heartbeat logs, day-by-day history).
//      The daily_notes table is the source of truth; triggers keep the
//      FTS virtual table in sync on insert/update/delete.
//
//   2. session_summaries_fts — references the existing sessions.summary
//      column via external-content mode. No data duplication; triggers on
//      the sessions table keep the index fresh.
//
// Both use the default unicode61 tokenizer, which handles CJK gracefully
// via Unicode code-point splitting — verified in TestFTS5Available with
// the Chinese phrase "心跳".
//
// Entry points:
//   - ensureFTSSchemas(db)  — idempotent schema creation + triggers
//   - indexDailyNotes()     — incremental reindex (mtime+hash skip)
//   - searchFTS(query, ...) — unified search, BM25-ranked
//   - fts-index / search-fts / fts-rebuild db subcommands

// ─── schemas ────────────────────────────────────────────────────────────────

const ftsDailyNotesSchema = `
CREATE TABLE IF NOT EXISTS daily_notes (
    date    TEXT PRIMARY KEY,
    path    TEXT NOT NULL,
    content TEXT NOT NULL,
    mtime   INTEGER NOT NULL,
    hash    TEXT NOT NULL
);`

const ftsDailyNotesVirtual = `
CREATE VIRTUAL TABLE IF NOT EXISTS daily_notes_fts
    USING fts5(date UNINDEXED, content, content=daily_notes, content_rowid=rowid);`

const ftsDailyNotesTriggers = `
CREATE TRIGGER IF NOT EXISTS daily_notes_ai AFTER INSERT ON daily_notes BEGIN
    INSERT INTO daily_notes_fts(rowid, date, content) VALUES (new.rowid, new.date, new.content);
END;

CREATE TRIGGER IF NOT EXISTS daily_notes_au AFTER UPDATE ON daily_notes BEGIN
    INSERT INTO daily_notes_fts(daily_notes_fts, rowid, date, content)
        VALUES('delete', old.rowid, old.date, old.content);
    INSERT INTO daily_notes_fts(rowid, date, content)
        VALUES (new.rowid, new.date, new.content);
END;

CREATE TRIGGER IF NOT EXISTS daily_notes_ad AFTER DELETE ON daily_notes BEGIN
    INSERT INTO daily_notes_fts(daily_notes_fts, rowid, date, content)
        VALUES('delete', old.rowid, old.date, old.content);
END;`

const ftsSessionSummariesVirtual = `
CREATE VIRTUAL TABLE IF NOT EXISTS session_summaries_fts
    USING fts5(path UNINDEXED, summary, content=sessions, content_rowid=rowid);`

const ftsSessionTriggers = `
CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN
    INSERT INTO session_summaries_fts(rowid, path, summary) VALUES (new.rowid, new.path, new.summary);
END;

CREATE TRIGGER IF NOT EXISTS sessions_au AFTER UPDATE ON sessions BEGIN
    INSERT INTO session_summaries_fts(session_summaries_fts, rowid, path, summary)
        VALUES('delete', old.rowid, old.path, old.summary);
    INSERT INTO session_summaries_fts(rowid, path, summary)
        VALUES (new.rowid, new.path, new.summary);
END;

CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN
    INSERT INTO session_summaries_fts(session_summaries_fts, rowid, path, summary)
        VALUES('delete', old.rowid, old.path, old.summary);
END;`

// ensureFTSSchemas is called from openDB alongside ensureSoulSessionTable.
// Idempotent: safe to call on every openDB.
func ensureFTSSchemas(db *sql.DB) error {
	// daily_notes base table
	if _, err := db.Exec(ftsDailyNotesSchema); err != nil {
		return fmt.Errorf("daily_notes table: %w", err)
	}
	// daily_notes FTS virtual table (external content)
	if _, err := db.Exec(ftsDailyNotesVirtual); err != nil {
		return fmt.Errorf("daily_notes_fts virtual: %w", err)
	}
	// daily_notes triggers
	for _, stmt := range splitSQL(ftsDailyNotesTriggers) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("daily_notes trigger: %w", err)
		}
	}
	// session_summaries FTS virtual table (external content over sessions)
	if _, err := db.Exec(ftsSessionSummariesVirtual); err != nil {
		return fmt.Errorf("session_summaries_fts virtual: %w", err)
	}
	// sessions triggers
	for _, stmt := range splitSQL(ftsSessionTriggers) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("sessions trigger: %w", err)
		}
	}
	// First-run rebuild for session_summaries_fts (in case sessions rows exist without index entries)
	// This is safe & cheap even on subsequent calls — the 'rebuild' command reseeds the index.
	var needRebuild int
	db.QueryRow(`SELECT CASE WHEN (SELECT COUNT(*) FROM sessions) > (SELECT COUNT(*) FROM session_summaries_fts) THEN 1 ELSE 0 END`).Scan(&needRebuild)
	if needRebuild == 1 {
		db.Exec(`INSERT INTO session_summaries_fts(session_summaries_fts) VALUES('rebuild')`)
	}
	return nil
}

// splitSQL breaks a multi-statement SQL string on semicolons (ignoring empty
// statements). SQLite's db.Exec accepts multiple statements in one call for
// most drivers, but modernc.org/sqlite is stricter about trigger bodies, so
// we split at the top-level statement boundary. Triggers themselves use
// BEGIN/END blocks and nested semicolons are kept together.
func splitSQL(s string) []string {
	// Our CREATE TRIGGER blocks terminate with "END;" — split on that
	// sentinel and then clean up.
	var out []string
	parts := strings.SplitAfter(s, "END;")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{strings.TrimSpace(s)}
	}
	return out
}

// ─── daily notes indexing ──────────────────────────────────────────────────

// dailyNoteDatePattern matches filenames like "2026-04-09.md"
var dailyNoteDatePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.md$`)

// indexDailyNotes walks workspace/memory/*.md and upserts daily notes.
// Unchanged files (matching mtime+hash) are skipped. Returns (added, skipped, err).
func indexDailyNotes() (int, int, error) {
	dir := filepath.Join(workspace, "memory")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, fmt.Errorf("read memory dir: %w", err)
	}

	db, err := openDB()
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()

	added, skipped := 0, 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dailyNoteDatePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		date := m[1]
		path := filepath.Join(dir, e.Name())

		info, err := e.Info()
		if err != nil {
			continue
		}
		mtime := info.ModTime().Unix()
		hash, _ := fileHash(path)
		if hash == "" {
			continue
		}

		// Skip unchanged files
		var existingHash string
		var existingMtime int64
		err = db.QueryRow(`SELECT hash, mtime FROM daily_notes WHERE date = ?`, date).Scan(&existingHash, &existingMtime)
		if err == nil && existingHash == hash && existingMtime == mtime {
			skipped++
			continue
		}

		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		// Strip YAML frontmatter for cleaner search (keeps body only)
		body := stripFrontmatter(string(content))

		_, err = db.Exec(
			`INSERT INTO daily_notes(date, path, content, mtime, hash) VALUES(?,?,?,?,?)
			 ON CONFLICT(date) DO UPDATE SET path=excluded.path, content=excluded.content, mtime=excluded.mtime, hash=excluded.hash`,
			date, path, body, mtime, hash,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] fts index %s: %v\n", appName, date, err)
			continue
		}
		added++
	}
	return added, skipped, nil
}

// stripFrontmatter removes YAML frontmatter from markdown content.
// Returns body only. If no frontmatter, returns content unchanged.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return s
	}
	// Find the closing ---
	rest := s[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		idx = strings.Index(rest, "\n---\r\n")
	}
	if idx < 0 {
		return s // malformed, return as-is
	}
	// skip past the closing delimiter
	body := rest[idx+5:]
	return strings.TrimLeft(body, "\r\n")
}

// rebuildFTS drops and recreates the FTS indexes from their underlying tables.
// Used as an escape hatch when schemas drift or index is corrupted.
func rebuildFTS() error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// daily_notes_fts rebuild
	if _, err := db.Exec(`INSERT INTO daily_notes_fts(daily_notes_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild daily_notes_fts: %w", err)
	}
	// session_summaries_fts rebuild
	if _, err := db.Exec(`INSERT INTO session_summaries_fts(session_summaries_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild session_summaries_fts: %w", err)
	}
	return nil
}

// ─── search ────────────────────────────────────────────────────────────────

type ftsHit struct {
	Source  string  `json:"source"`            // "daily" | "session"
	Date    string  `json:"date,omitempty"`    // daily: "2026-04-09"
	Path    string  `json:"path,omitempty"`    // session: session file path
	Snippet string  `json:"snippet"`           // FTS5 snippet() with [..] highlight
	Rank    float64 `json:"rank"`              // BM25 score (lower = better match)
}

// searchFTS runs MATCH against one or both FTS5 virtual tables.
// scope: "daily" | "session" | "both" (default).
// query: raw FTS5 MATCH expression. Users can pass a simple keyword or a
//        full FTS5 query syntax (phrases, NEAR, column filters).
// limit: max rows per source.
// sanitizeFTSQuery makes a user query safe for FTS5 MATCH by quoting each
// whitespace-separated token as a phrase. This handles punctuation like
// `GLM 5.1` or `#207` that would otherwise trip the FTS5 parser.
//
// Tokens are quoted with double-quotes; any embedded double-quote is escaped
// per FTS5 convention by doubling it. Empty input returns empty string.
func sanitizeFTSQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	fields := strings.Fields(q)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// Escape embedded double-quotes (FTS5: "" inside "...")
		f = strings.ReplaceAll(f, `"`, `""`)
		out = append(out, `"`+f+`"`)
	}
	return strings.Join(out, " ")
}

func searchFTS(query, scope string, limit int) ([]ftsHit, error) {
	if limit <= 0 {
		limit = 20
	}
	if scope == "" {
		scope = "both"
	}
	match := sanitizeFTSQuery(query)
	if match == "" {
		return nil, nil
	}
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var hits []ftsHit

	if scope == "daily" || scope == "both" {
		rows, err := db.Query(`
			SELECT date, snippet(daily_notes_fts, 1, '[', ']', ' … ', 24), rank
			FROM daily_notes_fts
			WHERE daily_notes_fts MATCH ?
			ORDER BY rank
			LIMIT ?`, match, limit)
		if err != nil {
			return nil, fmt.Errorf("daily search: %w", err)
		}
		for rows.Next() {
			var h ftsHit
			h.Source = "daily"
			rows.Scan(&h.Date, &h.Snippet, &h.Rank)
			hits = append(hits, h)
		}
		rows.Close()
	}

	if scope == "session" || scope == "both" {
		rows, err := db.Query(`
			SELECT path, snippet(session_summaries_fts, 1, '[', ']', ' … ', 24), rank
			FROM session_summaries_fts
			WHERE session_summaries_fts MATCH ?
			ORDER BY rank
			LIMIT ?`, match, limit)
		if err != nil {
			return nil, fmt.Errorf("session search: %w", err)
		}
		for rows.Next() {
			var h ftsHit
			h.Source = "session"
			rows.Scan(&h.Path, &h.Snippet, &h.Rank)
			hits = append(hits, h)
		}
		rows.Close()
	}

	return hits, nil
}

// ─── CLI handlers ──────────────────────────────────────────────────────────

// handleFTSIndex implements `weiran db fts-index`
func handleFTSIndex() {
	start := time.Now()
	added, skipped, err := indexDailyNotes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] fts-index: %v\n", appName, err)
		os.Exit(1)
	}
	fmt.Printf("fts-index: %d added/updated, %d skipped (%.2fs)\n", added, skipped, time.Since(start).Seconds())
}

// handleFTSRebuild implements `weiran db fts-rebuild`
func handleFTSRebuild() {
	if err := rebuildFTS(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] fts-rebuild: %v\n", appName, err)
		os.Exit(1)
	}
	fmt.Println("fts-rebuild: ok")
}

// handleFTSSearch implements `weiran db search-fts <query> [--scope=both|daily|session] [--limit=N] [--json]`
func handleFTSSearch(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: "+appName+" db search-fts <query> [--scope=daily|session|both] [--limit=N] [--json]")
		os.Exit(1)
	}
	scope := "both"
	limit := 20
	asJSON := false
	var queryParts []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--scope="):
			scope = strings.TrimPrefix(a, "--scope=")
		case strings.HasPrefix(a, "--limit="):
			fmt.Sscanf(strings.TrimPrefix(a, "--limit="), "%d", &limit)
		case a == "--json":
			asJSON = true
		default:
			queryParts = append(queryParts, a)
		}
	}
	query := strings.Join(queryParts, " ")
	if query == "" {
		fmt.Fprintln(os.Stderr, "empty query")
		os.Exit(1)
	}

	hits, err := searchFTS(query, scope, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] search-fts: %v\n", appName, err)
		os.Exit(1)
	}

	if asJSON {
		out, _ := json.MarshalIndent(hits, "", "  ")
		fmt.Println(string(out))
		return
	}

	if len(hits) == 0 {
		fmt.Printf("no matches for %q (scope=%s)\n", query, scope)
		return
	}
	for _, h := range hits {
		if h.Source == "daily" {
			fmt.Printf("\n📅 [%s] rank=%.2f\n   %s\n", h.Date, h.Rank, h.Snippet)
		} else {
			fmt.Printf("\n💬 %s rank=%.2f\n   %s\n", filepath.Base(h.Path), h.Rank, h.Snippet)
		}
	}
	fmt.Printf("\n%d hit(s)\n", len(hits))
}
