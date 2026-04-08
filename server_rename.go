package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ── Server Session DB (tracks rename state + user turns) ──

var serverDB *sql.DB
var serverDBOnce sync.Once
var serverDBMu sync.Mutex // serialize writes to SQLite

func openServerDB() (*sql.DB, error) {
	var err error
	serverDBOnce.Do(func() {
		dbFile := appDir + "/server_sessions.db"
		serverDB, err = sql.Open("sqlite", dbFile)
		if err != nil {
			return
		}
		serverDB.Exec(`PRAGMA journal_mode=WAL`)
		serverDB.Exec(`PRAGMA busy_timeout=5000`)
		_, err = serverDB.Exec(`CREATE TABLE IF NOT EXISTS server_sessions (
			session_id   TEXT PRIMARY KEY,
			name         TEXT NOT NULL DEFAULT '',
			renamed      INTEGER NOT NULL DEFAULT 0,
			user_turns   INTEGER NOT NULL DEFAULT 0,
			auto_named   INTEGER NOT NULL DEFAULT 0,
			created_at   TEXT NOT NULL,
			updated_at   TEXT NOT NULL
		)`)
		if err != nil {
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
	})
	if err != nil {
		return nil, err
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
