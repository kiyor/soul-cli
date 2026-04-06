package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	})
	if err != nil {
		return nil, err
	}
	return serverDB, nil
}

// ensureServerSession creates or updates the DB record for a session.
func ensureServerSession(sessionID, name string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	now := time.Now().Format(time.RFC3339)
	db.Exec(`INSERT INTO server_sessions (session_id, name, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET updated_at=?`,
		sessionID, name, now, now, now)
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

// callHaikuForTitle calls Claude Haiku to generate a short session title.
func callHaikuForTitle(context string) string {
	// Use claude CLI in one-shot mode with haiku
	// Simpler approach: call the Anthropic API directly via the proxy or local claude binary
	prompt := fmt.Sprintf(`Based on this conversation snippet, generate a very short title (3-8 words, no quotes, no punctuation at end). Just output the title, nothing else.

Conversation:
%s`, context)

	type haikuMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	apiKey := getAnthropicAPIKey()
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "[%s] server: auto-rename: no Anthropic API key found\n", appName)
		return ""
	}

	reqBody, _ := json.Marshal(struct {
		Model     string     `json:"model"`
		MaxTokens int        `json:"max_tokens"`
		Messages  []haikuMsg `json:"messages"`
	}{
		Model:     "claude-haiku-4-5-20251001",
		MaxTokens: 30,
		Messages: []haikuMsg{
			{Role: "user", Content: prompt},
		},
	})

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server: auto-rename API error: %v\n", appName, err)
		return ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "[%s] server: auto-rename API %d: %s\n", appName, resp.StatusCode, string(body))
		return ""
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &result) != nil {
		return ""
	}

	for _, c := range result.Content {
		if c.Type == "text" {
			title := strings.TrimSpace(c.Text)
			// Sanitize: remove quotes, limit length
			title = strings.Trim(title, `"'`)
			if len(title) > 60 {
				title = title[:60]
			}
			return title
		}
	}
	return ""
}

// getAnthropicAPIKey reads the API key from auth profiles.
func getAnthropicAPIKey() string {
	// Check env first
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key
	}

	// Read from openclaw auth profiles
	paths := []string{
		home + "/.openclaw/agents/main/agent/auth-profiles.json",
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// Try array format
		var profiles []struct {
			Provider string `json:"provider"`
			APIKey   string `json:"apiKey"`
		}
		if json.Unmarshal(data, &profiles) == nil {
			for _, prof := range profiles {
				if strings.Contains(strings.ToLower(prof.Provider), "anthropic") && prof.APIKey != "" {
					return prof.APIKey
				}
			}
		}
	}
	return ""
}
