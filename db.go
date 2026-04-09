package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── DB ──

func openDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open DB: %w", err)
	}
	// WAL mode: allows concurrent readers while writing, much better for multi-process access
	db.Exec("PRAGMA journal_mode=WAL")
	// Busy timeout: wait up to 5s for lock instead of failing immediately
	db.Exec("PRAGMA busy_timeout=5000")
	schemas := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			path       TEXT PRIMARY KEY,
			size       INTEGER NOT NULL,
			hash       TEXT NOT NULL,
			summary    TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS patterns (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			example     TEXT NOT NULL DEFAULT '',
			sources     TEXT NOT NULL DEFAULT '[]',
			first_seen  TEXT NOT NULL,
			last_seen   TEXT NOT NULL,
			seen_count  INTEGER NOT NULL DEFAULT 1,
			status      TEXT NOT NULL DEFAULT 'candidate'
		)`,
		`CREATE TABLE IF NOT EXISTS pattern_feedback (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			pattern_id INTEGER NOT NULL REFERENCES patterns(id),
			outcome    TEXT NOT NULL,
			note       TEXT NOT NULL DEFAULT '',
			session    TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_agents (
			claude_session_id TEXT PRIMARY KEY,
			agent_id          TEXT NOT NULL DEFAULT 'main',
			agent_name        TEXT NOT NULL DEFAULT '',
			source            TEXT NOT NULL DEFAULT '',
			created_at        TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS spawns (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			agent       TEXT NOT NULL,
			agent_name  TEXT NOT NULL DEFAULT '',
			task        TEXT NOT NULL,
			session     TEXT NOT NULL DEFAULT '',
			pid         INTEGER NOT NULL DEFAULT 0,
			exit_code   INTEGER DEFAULT NULL,
			duration_s  REAL DEFAULT NULL,
			log_path    TEXT NOT NULL DEFAULT '',
			output_tail TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT 'running',
			started_at  TEXT NOT NULL,
			finished_at TEXT DEFAULT NULL
		)`,
	}
	for _, s := range schemas {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, fmt.Errorf("schema creation failed: %w", err)
		}
	}
	// soul_sessions table (session lifecycle management)
	if err := ensureSoulSessionTable(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("soul_sessions schema failed: %w", err)
	}
	// FTS5 full-text search over daily notes and session summaries
	if err := ensureFTSSchemas(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("fts schema failed: %w", err)
	}
	return db, nil
}

// recordSessionAgent stores the agent identity for a Claude session ID.
func recordSessionAgent(claudeSID, agentID, agentName, source string) {
	db, err := openDB()
	if err != nil {
		return
	}
	defer db.Close()
	db.Exec(`INSERT OR REPLACE INTO session_agents (claude_session_id, agent_id, agent_name, source, created_at) VALUES (?, ?, ?, ?, ?)`,
		claudeSID, agentID, agentName, source, time.Now().Format(time.RFC3339))
}

// getSessionAgent returns the agent ID for a Claude session ID, or "" if unknown.
func getSessionAgent(claudeSID string) string {
	db, err := openDB()
	if err != nil {
		return ""
	}
	defer db.Close()
	var agent string
	db.QueryRow(`SELECT agent_id FROM session_agents WHERE claude_session_id=?`, claudeSID).Scan(&agent)
	return agent
}

// loadSpawnAgentMap returns a map of session_name → agent_id from the spawns table.
func loadSpawnAgentMap() map[string]string {
	db, err := openDB()
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT session, agent FROM spawns`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var session, agent string
		rows.Scan(&session, &agent)
		m[session] = agent
	}
	return m
}

func fileHash(path string) (string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", 0
	}
	size := info.Size()

	h := sha256.New()
	// large files: hash only first+last 64KB
	if size > 128*1024 {
		buf := make([]byte, 64*1024)
		n, err := f.Read(buf)
		if err != nil {
			return "", 0
		}
		h.Write(buf[:n])
		if _, err := f.Seek(-64*1024, io.SeekEnd); err != nil {
			return "", 0
		}
		n, _ = f.Read(buf)
		h.Write(buf[:n])
		// mix in size as well
		fmt.Fprintf(h, "%d", size)
	} else {
		io.Copy(h, f)
	}
	return hex.EncodeToString(h.Sum(nil)), size
}

type sessionState struct {
	path    string
	size    int64
	hash    string
	changed bool // true = new file or content changed
	summary string
}

func checkSessions(db *sql.DB, files []sessionFile) []sessionState {
	var states []sessionState
	for _, sf := range files {
		hash, size := fileHash(sf.path)
		if hash == "" {
			continue
		}

		st := sessionState{path: sf.path, size: size, hash: hash, changed: true}

		var dbHash string
		var dbSize int64
		var dbSummary string
		err := db.QueryRow("SELECT hash, size, summary FROM sessions WHERE path = ?", sf.path).
			Scan(&dbHash, &dbSize, &dbSummary)
		if err == nil && dbHash == hash && dbSize == size {
			st.changed = false
			st.summary = dbSummary
		}

		states = append(states, st)
	}
	return states
}

// saveSummary saves a single file's summary to DB
func saveSummary(db *sql.DB, path, hash string, size int64, summary string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`INSERT INTO sessions (path, size, hash, summary, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, hash=excluded.hash, summary=excluded.summary, updated_at=excluded.updated_at`,
		path, size, hash, summary, now)
	return err
}

// ── DB subcommands ──

func handleDB(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: " + appName + " db <save|list|stats>")
		return
	}
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	switch args[0] {
	case "save":
		// weiran db save '{"path":"...","summary":"..."}'
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db save '<json>'")
			os.Exit(1)
		}
		var input struct {
			Path    string `json:"path"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(args[1]), &input); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		hash, size := fileHash(input.Path)
		if hash == "" {
			fmt.Fprintf(os.Stderr, "file not readable: %s\n", input.Path)
			os.Exit(1)
		}
		if err := saveSummary(db, input.Path, hash, size, input.Summary); err != nil {
			fmt.Fprintf(os.Stderr, "save failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("saved: %s (%d bytes)\n", input.Path, size)

	case "save-batch":
		// ` + appName + ` db save-batch '<json array>'
		// save multiple at once: [{"path":"...","summary":"..."},...]
		// also supports reading from stdin (when arg is - or absent)
		var raw []byte
		if len(args) < 2 || args[1] == "-" {
			raw, _ = io.ReadAll(os.Stdin)
		} else {
			raw = []byte(args[1])
		}
		var inputs []struct {
			Path    string `json:"path"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(raw, &inputs); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		saved := 0
		for _, input := range inputs {
			hash, size := fileHash(input.Path)
			if hash == "" {
				fmt.Fprintf(os.Stderr, "skipping unreadable: %s\n", input.Path)
				continue
			}
			if err := saveSummary(db, input.Path, hash, size, input.Summary); err != nil {
				fmt.Fprintf(os.Stderr, "save failed for %s: %v\n", input.Path, err)
				continue
			}
			fmt.Printf("saved: %s (%d bytes)\n", input.Path, size)
			saved++
		}
		fmt.Printf("batch save complete: %d/%d\n", saved, len(inputs))

	case "list":
		rows, err := db.Query("SELECT path, size, hash, summary, updated_at FROM sessions ORDER BY updated_at DESC LIMIT 20")
		if err != nil {
			fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
			os.Exit(1)
		}
		defer rows.Close()
		for rows.Next() {
			var path, hash, summary, updatedAt string
			var size int64
			rows.Scan(&path, &size, &hash, &summary, &updatedAt)
			sumPreview := summary
			if len(sumPreview) > 80 {
				sumPreview = sumPreview[:80] + "..."
			}
			fmt.Printf("[%s] %s (%d bytes) hash=%s…\n  %s\n", updatedAt, path, size, hash[:12], sumPreview)
		}

	case "stats":
		var total, withSummary int
		db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&total)
		db.QueryRow("SELECT COUNT(*) FROM sessions WHERE summary != ''").Scan(&withSummary)
		fmt.Printf("total: %d sessions, %d with summary\n", total, withSummary)

	case "recall":
		// weiran db recall — output recall instructions (for claude), on demand
		sessions := recentSessions(20)
		states := checkSessions(db, sessions)
		var pending, done []sessionState
		for _, s := range states {
			if s.changed || s.summary == "" {
				pending = append(pending, s)
			} else {
				done = append(done, s)
			}
		}
		if len(pending) == 0 {
			fmt.Println("all recent sessions already have summaries, no scanning needed.")
			if len(done) > 0 {
				fmt.Println("\nwith summary:")
				for _, s := range done {
					fmt.Printf("  %s → %s\n", filepath.Base(s.path), s.summary)
				}
			}
			return
		}
		fmt.Printf("%d sessions to scan (%d already done).\n\n", len(pending), len(done))
		fmt.Println("files to scan:")
		for _, s := range pending {
			fmt.Printf("  %s (%d bytes)\n", s.path, s.size)
		}
		fmt.Println("\nscanning method:")
		fmt.Println("  - tail last 200-500 lines of each file")
		fmt.Println("  - OpenClaw JSONL：type \"message\", message.role \"user\"")
		fmt.Println("  - Claude Code JSONL：type \"user\", message.content")
		fmt.Println("  - focus on: owner's instructions, decisions, events, emotional exchanges")
		fmt.Println("\nsave after scanning:")
		fmt.Println("  ` + appName + ` db save-batch '[{\"path\":\"...\",\"summary\":\"...\"},...]'")

	case "pending":
		// weiran db pending — output sessions needing scan (JSON format, machine-readable)
		sessions := recentSessions(20)
		states := checkSessions(db, sessions)
		type pendingItem struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
			Hash string `json:"hash"`
		}
		var pending []pendingItem
		for _, s := range states {
			if s.changed || s.summary == "" {
				pending = append(pending, pendingItem{Path: s.path, Size: s.size, Hash: s.hash})
			}
		}
		out, _ := json.MarshalIndent(pending, "", "  ")
		fmt.Println(string(out))

	case "summarized":
		// weiran db summarized — output sessions with existing summaries (JSON)
		sessions := recentSessions(20)
		states := checkSessions(db, sessions)
		type sumItem struct {
			Path    string `json:"path"`
			Summary string `json:"summary"`
		}
		var items []sumItem
		for _, s := range states {
			if !s.changed && s.summary != "" {
				items = append(items, sumItem{Path: s.path, Summary: s.summary})
			}
		}
		out, _ := json.MarshalIndent(items, "", "  ")
		fmt.Println(string(out))

	case "gc":
		// weiran db gc — clean up DB records for sessions no longer on disk
		rows, err := db.Query("SELECT path FROM sessions")
		if err != nil {
			fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
			os.Exit(1)
		}
		var stale []string
		for rows.Next() {
			var path string
			rows.Scan(&path)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				stale = append(stale, path)
			}
		}
		rows.Close()
		if len(stale) == 0 {
			fmt.Println("nothing to clean, all DB records have matching files")
			return
		}
		for _, p := range stale {
			db.Exec("DELETE FROM sessions WHERE path = ?", p)
			fmt.Printf("  removed: %s\n", filepath.Base(p))
		}
		fmt.Printf("cleanup done: removed %d stale records\n", len(stale))

	case "search":
		// weiran db search <keyword> — search session summaries
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db search <keyword>")
			os.Exit(1)
		}
		keyword := strings.Join(args[1:], " ")
		rows, err := db.Query("SELECT path, summary, updated_at FROM sessions WHERE summary LIKE ? ORDER BY updated_at DESC LIMIT 20",
			"%"+keyword+"%")
		if err != nil {
			fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
			os.Exit(1)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var path, summary, updatedAt string
			rows.Scan(&path, &summary, &updatedAt)
			fmt.Printf("\n[%s] %s\n", updatedAt[:10], filepath.Base(path))
			// highlight matching lines
			for _, line := range strings.Split(summary, "\n") {
				if strings.Contains(strings.ToLower(line), strings.ToLower(keyword)) {
					fmt.Printf("  → %s\n", strings.TrimSpace(line))
				}
			}
			count++
		}
		if count == 0 {
			fmt.Printf("no matches containing \"%s\"\n", keyword)
		} else {
			fmt.Printf("\n%d matches\n", count)
		}

	case "patterns":
		jsonMode := len(args) > 1 && args[1] == "-j"
		handlePatterns(db, jsonMode)

	case "pattern-save":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db pattern-save '{\"name\":\"...\",\"description\":\"...\",\"example\":\"...\",\"source\":\"...\"}'")
			os.Exit(1)
		}
		var input patternInput
		if err := json.Unmarshal([]byte(args[1]), &input); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		upsertPattern(db, input)

	case "pattern-save-batch":
		var raw []byte
		if len(args) < 2 || args[1] == "-" {
			raw, _ = io.ReadAll(os.Stdin)
		} else {
			raw = []byte(args[1])
		}
		var inputs []patternInput
		if err := json.Unmarshal(raw, &inputs); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		for _, input := range inputs {
			upsertPattern(db, input)
		}
		fmt.Printf("batch save complete: %d items\n", len(inputs))

	case "feedback":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db feedback '{\"pattern\":\"...\",\"outcome\":\"success|failure|correction\",\"note\":\"...\",\"session\":\"...\"}'")
			os.Exit(1)
		}
		var input struct {
			Pattern string `json:"pattern"`
			Outcome string `json:"outcome"`
			Note    string `json:"note"`
			Session string `json:"session"`
		}
		if err := json.Unmarshal([]byte(args[1]), &input); err != nil {
			fmt.Fprintf(os.Stderr, "JSON parse error: %v\n", err)
			os.Exit(1)
		}
		if input.Pattern == "" || input.Outcome == "" {
			fmt.Fprintln(os.Stderr, "pattern and outcome required")
			os.Exit(1)
		}
		var pid int
		err := db.QueryRow("SELECT id FROM patterns WHERE name = ?", input.Pattern).Scan(&pid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pattern not found: %s\n", input.Pattern)
			os.Exit(1)
		}
		now := time.Now().Format(time.RFC3339)
		db.Exec("INSERT INTO pattern_feedback (pattern_id, outcome, note, session, created_at) VALUES (?, ?, ?, ?, ?)",
			pid, input.Outcome, input.Note, input.Session, now)
		fmt.Printf("recorded: %s → %s\n", input.Pattern, input.Outcome)

	case "cultivate":
		dryRun := len(args) > 1 && args[1] == "--dry-run"
		handleCultivate(db, dryRun)

	case "pattern-reject":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: "+appName+" db pattern-reject <name>")
			os.Exit(1)
		}
		res, err := db.Exec("UPDATE patterns SET status = 'rejected' WHERE name = ? AND status = 'candidate'", args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
			os.Exit(1)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fmt.Fprintf(os.Stderr, "no candidate pattern found: %s\n", args[1])
			os.Exit(1)
		}
		fmt.Printf("rejected: %s\n", args[1])

	case "fts-index":
		// weiran db fts-index — incrementally index daily notes into FTS5
		handleFTSIndex()

	case "fts-rebuild":
		// weiran db fts-rebuild — drop and recreate FTS5 indexes from source tables
		handleFTSRebuild()

	case "search-fts":
		// weiran db search-fts <query> [--scope=daily|session|both] [--limit=N] [--json]
		handleFTSSearch(args[1:])

	default:
		fmt.Printf("unknown subcommand: %s\nusage: %s db <recall|pending|summarized|save|save-batch|list|stats|search|search-fts|fts-index|fts-rebuild|gc|patterns|pattern-save|pattern-save-batch|feedback|cultivate|pattern-reject>\n", args[0], appName)
	}
}

// importSummaries reads summaries.json written by claude and imports into DB
func importSummaries() {
	data, err := os.ReadFile(sessionTmp("summaries.json"))
	if err != nil {
		fmt.Fprint(os.Stderr, "["+appName+"] no summaries file, skipping import\n")
		return
	}

	var items []struct {
		Path    string `json:"path"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] summaries JSON parse error: %v\n", err)
		return
	}

	if len(items) == 0 {
		return
	}

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] %v\n", err)
		return
	}
	defer db.Close()

	saved := 0
	for _, item := range items {
		hash, size := fileHash(item.Path)
		if hash == "" {
			continue
		}
		if err := saveSummary(db, item.Path, hash, size, item.Summary); err != nil {
			fmt.Fprintf(os.Stderr, "["+appName+"] save failed for %s: %v\n", item.Path, err)
			continue
		}
		saved++
	}
	fmt.Fprintf(os.Stderr, "["+appName+"] imported %d/%d summaries\n", saved, len(items))
}

// ── Pattern Cultivation ──

// quality gate thresholds
const (
	cultivateMinSeen      = 5
	cultivateMinDiversity = 2
	cultivateMinReliab    = 0.80
	cultivateMaxNegRate   = 0.25
)

type patternInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
	Source      string `json:"source"`
}

type patternRow struct {
	ID          int
	Name        string
	Description string
	Example     string
	Sources     []string
	FirstSeen   string
	LastSeen    string
	SeenCount   int
	Status      string
}

type patternMetrics struct {
	SuccessCount    int
	FailureCount    int
	CorrectionCount int
	TotalFeedback   int
	Reliability     float64 // success / (success+failure)
	NegativeRate    float64 // (failure+correction) / total
	Diversity       int     // len(sources)
}

func upsertPattern(db *sql.DB, input patternInput) {
	now := time.Now().Format(time.RFC3339)

	var existing struct {
		id      int
		sources string
		count   int
	}
	err := db.QueryRow("SELECT id, sources, seen_count FROM patterns WHERE name = ?", input.Name).
		Scan(&existing.id, &existing.sources, &existing.count)

	if err != nil {
		// new pattern
		sources, _ := json.Marshal([]string{input.Source})
		db.Exec(`INSERT INTO patterns (name, description, example, sources, first_seen, last_seen, seen_count, status)
			VALUES (?, ?, ?, ?, ?, ?, 1, 'candidate')`,
			input.Name, input.Description, input.Example, string(sources), now, now)
		fmt.Printf("new pattern: %s\n", input.Name)
		return
	}

	// update existing
	var srcList []string
	if err := json.Unmarshal([]byte(existing.sources), &srcList); err != nil {
		srcList = nil // reset on bad JSON
	}
	if input.Source != "" {
		found := false
		for _, s := range srcList {
			if s == input.Source {
				found = true
				break
			}
		}
		if !found {
			srcList = append(srcList, input.Source)
		}
	}
	srcJSON, _ := json.Marshal(srcList)

	desc := input.Description
	if desc == "" {
		desc = "" // keep DB value (don't overwrite with empty)
		db.Exec(`UPDATE patterns SET sources = ?, last_seen = ?, seen_count = seen_count + 1 WHERE id = ?`,
			string(srcJSON), now, existing.id)
	} else {
		db.Exec(`UPDATE patterns SET description = ?, sources = ?, last_seen = ?, seen_count = seen_count + 1 WHERE id = ?`,
			desc, string(srcJSON), now, existing.id)
	}
	if input.Example != "" {
		db.Exec(`UPDATE patterns SET example = ? WHERE id = ?`, input.Example, existing.id)
	}
	fmt.Printf("updated pattern: %s (seen=%d)\n", input.Name, existing.count+1)
}

func getPatternMetrics(db *sql.DB, patternID int, sources []string) patternMetrics {
	var m patternMetrics
	m.Diversity = len(sources)

	rows, err := db.Query("SELECT outcome, COUNT(*) FROM pattern_feedback WHERE pattern_id = ? GROUP BY outcome", patternID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var outcome string
			var count int
			if rows.Scan(&outcome, &count) == nil {
				switch outcome {
				case "success":
					m.SuccessCount = count
				case "failure":
					m.FailureCount = count
				case "correction":
					m.CorrectionCount = count
				}
			}
		}
	}
	m.TotalFeedback = m.SuccessCount + m.FailureCount + m.CorrectionCount

	if denom := m.SuccessCount + m.FailureCount; denom > 0 {
		m.Reliability = float64(m.SuccessCount) / float64(denom)
	}
	if m.TotalFeedback > 0 {
		m.NegativeRate = float64(m.FailureCount+m.CorrectionCount) / float64(m.TotalFeedback)
	}
	return m
}

func handlePatterns(db *sql.DB, jsonMode bool) {
	rows, err := db.Query("SELECT id, name, description, sources, first_seen, last_seen, seen_count, status FROM patterns WHERE status != 'archived' ORDER BY last_seen DESC")
	if err != nil {
		fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
		return
	}
	defer rows.Close()

	type patternOut struct {
		ID          int            `json:"id"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		SeenCount   int            `json:"seen_count"`
		Status      string         `json:"status"`
		LastSeen    string         `json:"last_seen"`
		Metrics     patternMetrics `json:"metrics"`
	}
	var items []patternOut

	for rows.Next() {
		var p patternOut
		var sourcesJSON string
		var firstSeen string
		rows.Scan(&p.ID, &p.Name, &p.Description, &sourcesJSON, &firstSeen, &p.LastSeen, &p.SeenCount, &p.Status)
		var sources []string
		json.Unmarshal([]byte(sourcesJSON), &sources)
		p.Metrics = getPatternMetrics(db, p.ID, sources)
		items = append(items, p)
	}

	if jsonMode {
		out, _ := json.MarshalIndent(items, "", "  ")
		fmt.Println(string(out))
		return
	}

	if len(items) == 0 {
		fmt.Println("no patterns yet")
		return
	}
	for _, p := range items {
		gate := "❌"
		if p.SeenCount >= cultivateMinSeen && p.Metrics.Diversity >= cultivateMinDiversity &&
			p.Metrics.Reliability >= cultivateMinReliab && p.Metrics.NegativeRate <= cultivateMaxNegRate &&
			p.Metrics.TotalFeedback > 0 {
			gate = "✅"
		}
		fmt.Printf("[%s] %s  seen=%d div=%d reliab=%.2f neg=%.2f status=%s %s\n",
			p.Status, p.Name, p.SeenCount, p.Metrics.Diversity,
			p.Metrics.Reliability, p.Metrics.NegativeRate, p.Status, gate)
	}
}

func handleCultivate(db *sql.DB, dryRun bool) {
	rows, err := db.Query("SELECT id, name, description, example, sources, seen_count FROM patterns WHERE status = 'candidate'")
	if err != nil {
		fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
		return
	}

	// collect all candidates first, close rows before operating
	type candidate struct {
		id, seenCount       int
		name, desc, example string
		sourcesJSON         string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		rows.Scan(&c.id, &c.name, &c.desc, &c.example, &c.sourcesJSON, &c.seenCount)
		candidates = append(candidates, c)
	}
	rows.Close()

	var promoted int
	for _, c := range candidates {
		var sources []string
		json.Unmarshal([]byte(c.sourcesJSON), &sources)
		m := getPatternMetrics(db, c.id, sources)

		// quality gate
		if c.seenCount < cultivateMinSeen || m.Diversity < cultivateMinDiversity ||
			m.TotalFeedback == 0 || m.Reliability < cultivateMinReliab || m.NegativeRate > cultivateMaxNegRate {
			continue
		}

		if dryRun {
			fmt.Printf("🌱 promotable: %s (seen=%d div=%d reliab=%.2f neg=%.2f)\n",
				c.name, c.seenCount, m.Diversity, m.Reliability, m.NegativeRate)
			continue
		}

		// generate SKILL.md
		skillDir := filepath.Join(appHome, "skills", c.name)
		os.MkdirAll(skillDir, 0755)
		skillPath := filepath.Join(skillDir, "SKILL.md")
		content := generateSkillMD(c.name, c.desc, c.example, sources, c.seenCount, m)
		if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write SKILL.md: %v\n", err)
			continue
		}

		// update status
		db.Exec("UPDATE patterns SET status = 'promoted' WHERE id = ?", c.id)
		promoted++
		fmt.Printf("🎉 promoted: %s → %s\n", c.name, skillPath)
	}

	if dryRun {
		return
	}
	if promoted == 0 {
		fmt.Println("no patterns reached promotion threshold")
	} else {
		fmt.Printf("promoted %d skills this run\n", promoted)
	}
}

func generateSkillMD(name, desc, example string, sources []string, seenCount int, m patternMetrics) string {
	now := time.Now().Format(time.RFC3339)
	sourceList := ""
	for _, s := range sources {
		sourceList += fmt.Sprintf("- %s\n", filepath.Base(s))
	}
	return fmt.Sprintf(`---
name: %s
description: |
  %s
  Auto-cultivated from %d observations.
cultivated: true
cultivated_at: %s
---

# %s

%s

## Example

%s

## Quality

- Seen: %d times
- Source diversity: %d sessions
- Reliability: %.0f%%
- Negative rate: %.0f%%

## Origin

Auto-cultivated from %d session observations.

%s`, name, desc, seenCount, now, name, desc, example, seenCount, m.Diversity,
		m.Reliability*100, m.NegativeRate*100, seenCount, sourceList)
}

// importPatterns reads patterns.json written by claude, batch upsert
func importPatterns() {
	data, err := os.ReadFile(sessionTmp("patterns.json"))
	if err != nil {
		return // no file, normal
	}
	var inputs []patternInput
	if err := json.Unmarshal(data, &inputs); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] patterns JSON parse error: %v\n", err)
		return
	}
	if len(inputs) == 0 {
		return
	}
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] %v\n", err)
		return
	}
	defer db.Close()
	for _, input := range inputs {
		upsertPattern(db, input)
	}
	fmt.Fprintf(os.Stderr, "["+appName+"] imported %d patterns\n", len(inputs))
}
