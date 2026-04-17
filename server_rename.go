package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ── Server Session DB (tracks rename state + user turns) ──

var serverDB *sql.DB
var serverDBOnce sync.Once
var serverDBMu sync.Mutex  // serialize writes to SQLite
var serverDBErr error       // persisted across calls so sync.Once + error works correctly

func openServerDB() (*sql.DB, error) {
	serverDBOnce.Do(func() {
		dbFile := appDir + "/server_sessions.db"
		serverDB, serverDBErr = sql.Open("sqlite", dbFile)
		if serverDBErr != nil {
			return
		}
		serverDB.Exec(`PRAGMA journal_mode=WAL`)
		serverDB.Exec(`PRAGMA busy_timeout=5000`)
		_, serverDBErr = serverDB.Exec(`CREATE TABLE IF NOT EXISTS server_sessions (
			session_id   TEXT PRIMARY KEY,
			name         TEXT NOT NULL DEFAULT '',
			renamed      INTEGER NOT NULL DEFAULT 0,
			user_turns   INTEGER NOT NULL DEFAULT 0,
			auto_named   INTEGER NOT NULL DEFAULT 0,
			created_at   TEXT NOT NULL,
			updated_at   TEXT NOT NULL
		)`)
		if serverDBErr != nil {
			return
		}
		// Migration: add chrome_enabled column (idempotent — ignore "duplicate column" error)
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN chrome_enabled INTEGER NOT NULL DEFAULT 0`)
		// Migration: add gal_id column for GAL save resume
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN gal_id TEXT NOT NULL DEFAULT ''`)
		// Migration: add category and claude_session_id for session classification
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN category TEXT NOT NULL DEFAULT 'interactive'`)
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN claude_session_id TEXT NOT NULL DEFAULT ''`)
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN tags TEXT NOT NULL DEFAULT '[]'`)
		// Migration: add replace_soul (本我模式) — when true, spawn claude with
		// --system-prompt-file instead of --append-system-prompt-file, stripping
		// the CC native harness and leaving only the soul prompt.
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN replace_soul INTEGER NOT NULL DEFAULT 0`)
		// Migration: add soul_enabled — when false, session runs as pure CC (no
		// soul prompt attached, equivalent to `weiran spawn --bare`). Default 1
		// preserves historical semantics (sessions created before this column
		// existed had soul enabled).
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN soul_enabled INTEGER NOT NULL DEFAULT 1`)
		// Migration: add total_cost_usd — persisted from proxy log aggregation
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN total_cost_usd REAL NOT NULL DEFAULT 0`)

		// Migration: add participants for IPC tracking (JSON array of session IDs that sent messages)
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN participants TEXT NOT NULL DEFAULT '[]'`)

		// Migration: add status for session rehydration across server restarts
		// active = running, suspended = graceful shutdown, ended = destroyed/reaped
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`)
		// Migration: add rehydrate_message — non-empty means this session triggered the restart
		// and should receive this message after rehydration to continue working
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN rehydrate_message TEXT NOT NULL DEFAULT ''`)
		// Migration: add model — persisted for rehydration (e.g. "claude-opus-4-6[1m]")
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN model TEXT NOT NULL DEFAULT ''`)

		// Migration: add spawned_by for parent-child session tracking
		serverDB.Exec(`ALTER TABLE server_sessions ADD COLUMN spawned_by TEXT NOT NULL DEFAULT ''`)

		// Migration: session_interactions table for IPC anti-loop tracking
		serverDB.Exec(`CREATE TABLE IF NOT EXISTS session_interactions (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			from_id     TEXT NOT NULL,
			to_id       TEXT NOT NULL,
			snippet     TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL
		)`)
		serverDB.Exec(`CREATE INDEX IF NOT EXISTS idx_si_pair ON session_interactions(from_id, to_id)`)
	})
	if serverDBErr != nil {
		return nil, serverDBErr
	}
	return serverDB, nil
}

// ensureServerSession creates or updates the DB record for a session.
func ensureServerSession(sessionID, name string) {
	ensureServerSessionFull(sessionID, name, CategoryInteractive, nil)
}

// ensureServerSessionFull creates or updates with category and tags.
func ensureServerSessionFull(sessionID, name, category string, tags []string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	tagsJSON, _ := json.Marshal(tags)
	if category == "" {
		category = CategoryInteractive
	}
	db.Exec(`INSERT INTO server_sessions (session_id, name, category, tags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET updated_at=?`,
		sessionID, name, category, string(tagsJSON), now, now, now)
}

// setClaudeSessionID records the Claude Code session ID for a weiran session.
func setClaudeSessionID(sessionID, claudeSID string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	db.Exec(`UPDATE server_sessions SET claude_session_id=?, updated_at=? WHERE session_id=?`,
		claudeSID, time.Now().Format(time.RFC3339), sessionID)
}

// resolveResumeIDs disambiguates an input id into (weiranID, ccID).
//
// Callers of resumeSession historically passed a Claude Code session id as the
// "sessionID" parameter. With weiran-id-stable-on-resume, the preferred input
// is a weiran id. This helper accepts either and returns both so the caller
// can reuse the weiran id (keeping UI bookmarks / IPC keys stable) while still
// handing the correct cc id to `claude --resume`.
//
// Lookup order:
//  1. input matches server_sessions.session_id  → weiran id known, cc id from row
//  2. input matches server_sessions.claude_session_id → reverse lookup weiran id
//  3. no match → treat input as cc id (legacy), mint a fresh weiran id
//
// When multiple weiran rows share the same cc id (normal case — every prior
// resume generated a new weiran row with the same cc id), pick the most
// recently updated one. That row is the "current" weiran identity for that
// conversation.
func resolveResumeIDs(input string) (weiranID, ccID string, isNew bool) {
	if input == "" {
		return "", "", true
	}
	db, err := openServerDB()
	if err != nil {
		return input, input, true
	}
	// Case 1: input is a weiran id
	var cc string
	err = db.QueryRow(`SELECT claude_session_id FROM server_sessions WHERE session_id=?`, input).Scan(&cc)
	if err == nil {
		return input, cc, false
	}
	// Case 2: input is a cc id — find the most recent weiran session pointing at it
	var wid string
	err = db.QueryRow(`SELECT session_id FROM server_sessions
		WHERE claude_session_id=?
		ORDER BY updated_at DESC LIMIT 1`, input).Scan(&wid)
	if err == nil {
		return wid, input, false
	}
	// Case 3: unknown — legacy path, generate new weiran id
	return uuid.New().String(), input, true
}

// getSessionCategory returns the category for a session by its weiran session ID or Claude session ID.
func getSessionCategory(id string) string {
	db, err := openServerDB()
	if err != nil {
		return ""
	}
	var cat string
	db.QueryRow(`SELECT category FROM server_sessions WHERE session_id=? OR claude_session_id=?`, id, id).Scan(&cat)
	return cat
}

// getSessionCategoryByClaudeSID returns the category for a session by its Claude session ID.
func getSessionCategoryByClaudeSID(claudeSID string) string {
	db, err := openServerDB()
	if err != nil {
		return ""
	}
	var cat string
	db.QueryRow(`SELECT category FROM server_sessions WHERE claude_session_id=?`, claudeSID).Scan(&cat)
	return cat
}

// inferCategoryFromName guesses the category of a legacy session (no DB record)
// based on its name or first message content. Used as a fallback in /api/history.
func inferCategoryFromName(name, firstMsg string) string {
	combined := name + " " + firstMsg
	switch {
	case strings.Contains(combined, "Memory Consolidation (cron mode)") ||
		strings.Contains(name, "-cron-"):
		return CategoryCron
	case strings.Contains(combined, "Execute heartbeat patrol") ||
		strings.Contains(combined, "cron-heartbeat") ||
		strings.Contains(name, "-heartbeat-") ||
		strings.Contains(name, "heartbeat-"):
		return CategoryHeartbeat
	case strings.Contains(combined, "Self-Evolution (evolve mode") ||
		strings.Contains(name, "-evolve-"):
		return CategoryEvolve
	default:
		return CategoryInteractive
	}
}

// getSessionProxyCost queries the proxy DB for aggregated cost of a session.
func getSessionProxyCost(sessionID string) float64 {
	if proxyDB == nil || sessionID == "" {
		return 0
	}
	var cost float64
	proxyDB.QueryRow("SELECT COALESCE(SUM(cost_usd),0) FROM proxy_requests WHERE session_id=?", sessionID).Scan(&cost)
	return cost
}

// persistSessionCost writes the current proxy-aggregated cost to server_sessions DB.
func persistSessionCost(sessionID string) {
	cost := getSessionProxyCost(sessionID)
	if cost <= 0 {
		return
	}
	db, err := openServerDB()
	if err != nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET total_cost_usd=?, updated_at=? WHERE session_id=?`,
		cost, now, sessionID)
}

// getPersistedSessionCost reads the cached cost from server_sessions DB.
func getPersistedSessionCost(sessionID string) float64 {
	db, err := openServerDB()
	if err != nil {
		return 0
	}
	var cost float64
	db.QueryRow(`SELECT total_cost_usd FROM server_sessions WHERE session_id=?`, sessionID).Scan(&cost)
	return cost
}

// getWeiranSessionIDByClaudeSID returns the weiran session ID for a given Claude session ID.
func getWeiranSessionIDByClaudeSID(claudeSID string) string {
	db, err := openServerDB()
	if err != nil {
		return ""
	}
	var sid string
	db.QueryRow(`SELECT session_id FROM server_sessions WHERE claude_session_id=?`, claudeSID).Scan(&sid)
	return sid
}

// getSessionCostByClaudeSID returns the proxy-aggregated cost for a session identified by Claude session ID.
func getSessionCostByClaudeSID(claudeSID string) float64 {
	weiranSID := getWeiranSessionIDByClaudeSID(claudeSID)
	if weiranSID == "" {
		return getPersistedSessionCost(claudeSID) // fallback: try direct lookup
	}
	// Try live proxy cost first, then persisted
	cost := getSessionProxyCost(weiranSID)
	if cost > 0 {
		return cost
	}
	return getPersistedSessionCost(weiranSID)
}

// markRenamed sets the renamed flag so Haiku won't auto-rename.
func markRenamed(sessionID, newName string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET name=?, renamed=1, updated_at=? WHERE session_id=?`,
		newName, now, sessionID)
}

// incrementUserTurns bumps the user turn counter and returns the new count.
// Also returns whether the session has been manually renamed.
func incrementUserTurns(sessionID string) (turns int, renamed bool) {
	db, err := openServerDB()
	if err != nil {
		return 0, false
	}
	serverDBMu.Lock()
	defer serverDBMu.Unlock()
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET user_turns=user_turns+1, updated_at=? WHERE session_id=?`,
		now, sessionID)
	row := db.QueryRow(`SELECT user_turns, renamed FROM server_sessions WHERE session_id=?`, sessionID)
	row.Scan(&turns, &renamed)
	return
}

// markAutoNamed sets auto_named flag after Haiku renames.
func markAutoNamed(sessionID, newName string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET name=?, auto_named=1, updated_at=? WHERE session_id=?`,
		newName, now, sessionID)
}

// setChromeEnabled persists the chrome flag for a session.
func setChromeEnabled(sessionID string, enabled bool) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	v := 0
	if enabled {
		v = 1
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET chrome_enabled=?, updated_at=? WHERE session_id=?`,
		v, now, sessionID)
}

// setGalID persists the gal_id for a session.
func setGalID(sessionID, galID string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET gal_id=?, updated_at=? WHERE session_id=?`,
		galID, now, sessionID)
}

// getGalID reads the gal_id for a session.
func getGalID(sessionID string) string {
	db, err := openServerDB()
	if err != nil {
		return ""
	}
	var v string
	db.QueryRow(`SELECT gal_id FROM server_sessions WHERE session_id=?`, sessionID).Scan(&v)
	return v
}

// getChromeEnabled reads the chrome flag for a session.
func getChromeEnabled(sessionID string) bool {
	db, err := openServerDB()
	if err != nil {
		return false
	}
	var v int
	db.QueryRow(`SELECT chrome_enabled FROM server_sessions WHERE session_id=?`, sessionID).Scan(&v)
	return v == 1
}

// setReplaceSoulEnabled persists the 本我模式 flag for a session.
func setReplaceSoulEnabled(sessionID string, enabled bool) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	v := 0
	if enabled {
		v = 1
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET replace_soul=?, updated_at=? WHERE session_id=?`,
		v, now, sessionID)
}

// getReplaceSoulEnabled reads the 本我模式 flag for a session.
// Looks up by weiran session ID OR original Claude session ID, so a resume
// can inherit the original session's mode.
func getReplaceSoulEnabled(sessionID string) bool {
	db, err := openServerDB()
	if err != nil {
		return false
	}
	var v int
	db.QueryRow(`SELECT replace_soul FROM server_sessions WHERE session_id=? OR claude_session_id=?`, sessionID, sessionID).Scan(&v)
	return v == 1
}

// setSoulEnabledDB persists the soul_enabled flag for a session (false → bare/CC mode).
func setSoulEnabledDB(sessionID string, enabled bool) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	v := 0
	if enabled {
		v = 1
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET soul_enabled=?, updated_at=? WHERE session_id=?`,
		v, now, sessionID)
}

// getSoulEnabledDB reads the soul_enabled flag. Defaults true if row missing
// (preserves pre-migration semantics).
func getSoulEnabledDB(sessionID string) bool {
	db, err := openServerDB()
	if err != nil {
		return true
	}
	var v int = 1
	db.QueryRow(`SELECT soul_enabled FROM server_sessions WHERE session_id=? OR claude_session_id=?`, sessionID, sessionID).Scan(&v)
	return v == 1
}

// clearSessionRow is a no-op now — we keep session rows for history/category lookup.
// Previously deleted the row; now we preserve it so history can filter by category.
func clearSessionRow(sessionID string) {
	// Intentionally kept empty — session metadata persists for history queries.
}

// isAutoNamed checks if session was already auto-named by Haiku.
func isAutoNamed(sessionID string) bool {
	db, err := openServerDB()
	if err != nil {
		return false
	}
	var autoNamed int
	db.QueryRow(`SELECT auto_named FROM server_sessions WHERE session_id=?`, sessionID).Scan(&autoNamed)
	return autoNamed == 1
}

// isManuallyRenamed reports whether the user has explicitly renamed the session
// via the rename API. Used to decide whether name sync from Claude Code's
// session metadata is allowed to overwrite the current name.
func isManuallyRenamed(sessionID string) bool {
	db, err := openServerDB()
	if err != nil {
		return false
	}
	var renamed int
	db.QueryRow(`SELECT renamed FROM server_sessions WHERE session_id=?`, sessionID).Scan(&renamed)
	return renamed == 1
}

// ── Haiku Auto-Rename ──

// shouldAutoRename returns true if session qualifies for auto-rename check.
// Conditions: not manually renamed, user_turns is a multiple of 5, not already auto-named.
func shouldAutoRename(sessionID string) bool {
	db, err := openServerDB()
	if err != nil {
		return false
	}
	var turns, renamed, autoNamed int
	row := db.QueryRow(`SELECT user_turns, renamed, auto_named FROM server_sessions WHERE session_id=?`, sessionID)
	if row.Scan(&turns, &renamed, &autoNamed) != nil {
		return false
	}
	if renamed == 1 {
		return false // manually renamed, never auto-rename
	}
	if autoNamed == 1 {
		return false // already auto-named once
	}
	return turns > 0 && turns%5 == 0
}

// tryAutoRename calls Haiku to generate a short session title based on conversation context.
// Runs in a goroutine — non-blocking.
func tryAutoRename(sess *serverSession) {
	if sess == nil {
		return
	}

	// Collect recent assistant text from broadcaster history
	sess.broadcaster.mu.RLock()
	var snippets []string
	for _, ev := range sess.broadcaster.history {
		if ev.Event == "assistant" {
			var peek struct {
				Message struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(ev.Data, &peek) == nil {
				for _, c := range peek.Message.Content {
					if c.Type == "text" && c.Text != "" {
						text := c.Text
						if len(text) > 200 {
							text = text[:200]
						}
						snippets = append(snippets, text)
					}
				}
			}
		}
	}
	sess.broadcaster.mu.RUnlock()

	if len(snippets) == 0 {
		return
	}

	// Keep context short
	context := strings.Join(snippets, "\n---\n")
	if len(context) > 1500 {
		context = context[:1500]
	}

	title := callHaikuForTitle(context)
	if title == "" {
		return
	}

	// Update session name
	sess.mu.Lock()
	sess.Name = title
	sess.mu.Unlock()

	markAutoNamed(sess.ID, title)

	// Notify WS clients about the name change
	if sess.hub != nil {
		sess.hub.notifySessions()
	}

	fmt.Fprintf(os.Stderr, "[%s] server: auto-renamed %s → %q\n", appName, shortID(sess.ID), title)
}

// callHaikuForTitle uses the persistent haiku pool to generate a short session title.
func callHaikuForTitle(context string) string {
	prompt := fmt.Sprintf(`Based on this conversation snippet, generate a very short title (3-8 words, no quotes, no punctuation at end). Just output the title, nothing else.

Conversation:
%s`, context)

	result, err := getHaikuPool().query(prompt, haikuPoolQueryTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server: auto-rename haiku pool error: %v\n", appName, err)
		return ""
	}

	title := strings.TrimSpace(result)
	title = strings.Trim(title, `"'`)
	if len(title) > 60 {
		title = title[:60]
	}
	if title == "" {
		return ""
	}
	return title
}

// ── Session Rehydration DB helpers ──

// updateSessionStatus sets the persistent status for a session.
func updateSessionStatus(sessionID, status string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET status=?, updated_at=? WHERE session_id=?`,
		status, now, sessionID)
}

// batchSuspendActiveSessions marks all active sessions as suspended (for graceful shutdown).
func batchSuspendActiveSessions() int {
	db, err := openServerDB()
	if err != nil {
		return 0
	}
	now := time.Now().Format(time.RFC3339)
	res, err := db.Exec(`UPDATE server_sessions SET status='suspended', updated_at=? WHERE status='active'`, now)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// sessionMeta holds batch-loaded metadata from server_sessions DB.
type sessionMeta struct {
	WeiranSID string
	Category  string
	Model     string
	Cost      float64
}

// batchLoadSessionMeta loads metadata for the given session IDs only (matched against
// both session_id and claude_session_id columns via WHERE IN).
func batchLoadSessionMeta(ids []string) map[string]sessionMeta {
	if len(ids) == 0 {
		return nil
	}
	db, err := openServerDB()
	if err != nil {
		return nil
	}
	ph := make([]string, len(ids))
	for i := range ph {
		ph[i] = "?"
	}
	in := strings.Join(ph, ",")
	// Build args: same IDs used for both IN clauses
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	// Duplicate for the second IN clause
	allArgs := append(args, args...)
	rows, err := db.Query(fmt.Sprintf(
		`SELECT session_id, claude_session_id, COALESCE(category,''), COALESCE(model,''), COALESCE(total_cost_usd,0)
		 FROM server_sessions WHERE session_id IN (%s) OR claude_session_id IN (%s)`, in, in), allArgs...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	m := make(map[string]sessionMeta, len(ids))
	for rows.Next() {
		var sid, csid, cat, model string
		var cost float64
		if rows.Scan(&sid, &csid, &cat, &model, &cost) == nil {
			meta := sessionMeta{WeiranSID: sid, Category: cat, Model: model, Cost: cost}
			if csid != "" {
				m[csid] = meta
			}
			m[sid] = meta
		}
	}
	return m
}

// setSessionModel persists the model name for a session.
// getSessionModel returns the persisted model for a session (by weiran session ID or Claude session ID).
func getSessionModel(id string) string {
	db, err := openServerDB()
	if err != nil {
		return ""
	}
	var model string
	db.QueryRow(`SELECT COALESCE(model,'') FROM server_sessions WHERE session_id=? OR claude_session_id=?`, id, id).Scan(&model)
	return model
}

func setSessionModel(sessionID, model string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET model=?, updated_at=? WHERE session_id=?`,
		model, now, sessionID)
}

// getModelFromJSONL extracts the model from a session's JSONL init message.
// This is a last-resort fallback when the model is not in the DB.
// The init message is typically within the first 5 lines; we scan up to 50 as safety margin.
func getModelFromJSONL(sessionID string) string {
	path := findSessionJSONL(sessionID)
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for i := 0; i < 50 && scanner.Scan(); i++ {
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) == nil && ev.Type == "system" && ev.Subtype == "init" && ev.Model != "" {
			return ev.Model
		}
	}
	return ""
}

// getFirstMsgFromJSONL reads the first user message from a session's JSONL file.
func getFirstMsgFromJSONL(sessionID string) string {
	path := findSessionJSONL(sessionID)
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) == nil && ev.Type == "user" && len(ev.Message.Content) > 0 {
			return extractText(ev.Message.Content)
		}
	}
	return ""
}

// setRehydrateMessage marks a session to be woken with a message after server restart.
func setRehydrateMessage(sessionID, message string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET rehydrate_message=?, updated_at=? WHERE session_id=?`,
		message, now, sessionID)
}

// rehydratableSession holds the info needed to resume a session after restart.
type rehydratableSession struct {
	SessionID       string
	ClaudeSID       string
	Name            string
	Category        string
	Model           string
	ReplaceSoul     bool
	SoulEnabled     bool
	RehydrateMsg    string
	ChromeEnabled   bool
	GalID           string
}

// getRehydratableSessions returns sessions eligible for rehydration.
// Criteria: status active/suspended, has claude_session_id, interactive/telegram category,
// updated within the last maxAge.
func getRehydratableSessions(maxAge time.Duration) ([]rehydratableSession, error) {
	db, err := openServerDB()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-maxAge).Format(time.RFC3339)
	rows, err := db.Query(`
		SELECT session_id, claude_session_id, name, category, replace_soul,
		       rehydrate_message, chrome_enabled, gal_id, model, soul_enabled
		FROM server_sessions
		WHERE status IN ('active', 'suspended')
		  AND claude_session_id != ''
		  AND category IN ('interactive', 'telegram')
		  AND updated_at > ?
		ORDER BY updated_at DESC
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []rehydratableSession
	for rows.Next() {
		var s rehydratableSession
		var replaceSoul, chrome, soulEnabled int
		if err := rows.Scan(&s.SessionID, &s.ClaudeSID, &s.Name, &s.Category,
			&replaceSoul, &s.RehydrateMsg, &chrome, &s.GalID, &s.Model, &soulEnabled); err != nil {
			continue
		}
		s.ReplaceSoul = replaceSoul == 1
		s.ChromeEnabled = chrome == 1
		s.SoulEnabled = soulEnabled == 1
		result = append(result, s)
	}
	return result, nil
}

// expireStaleRehydratables marks old active/suspended sessions as ended.
func expireStaleRehydratables(maxAge time.Duration) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge).Format(time.RFC3339)
	db.Exec(`UPDATE server_sessions SET status='ended' WHERE status IN ('active','suspended') AND updated_at <= ?`, cutoff)
}

// filterNestedClaudeEnv removes env vars that would confuse a nested claude subprocess.
func filterNestedClaudeEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		// Skip vars that indicate we're already inside a claude session
		if strings.HasPrefix(e, "CLAUDE_CODE_SESSION=") ||
			strings.HasPrefix(e, "CLAUDE_CODE_ENTRY_POINT=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}
