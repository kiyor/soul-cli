package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ── Session ──

// serverSession represents one active Claude Code session.
type serverSession struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Project     string    `json:"project"`
	Model       string    `json:"model,omitempty"`
	Status      string    `json:"status"` // starting, running, idle, stopped, error
	CreatedAt   time.Time `json:"created_at"`
	LastActive  time.Time `json:"last_active"`
	TotalCost   float64   `json:"total_cost_usd"`
	NumTurns    int       `json:"num_turns"`
	ClaudeSID   string    `json:"claude_session_id,omitempty"` // Claude Code's own session ID
	StreamURL   string    `json:"stream_url"`
	SoulEnabled bool      `json:"soul_enabled"`

	process     *claudeProcess
	broadcaster *sseBroadcaster
	promptFile  string // temp file for soul prompt
	hub         *wsHub // for WS notifications on status change
	mu          sync.Mutex
}

// touch updates LastActive timestamp.
func (s *serverSession) touch() {
	s.mu.Lock()
	s.LastActive = time.Now()
	s.mu.Unlock()
}

// setStatus atomically updates session status and notifies WS clients.
func (s *serverSession) setStatus(status string) {
	s.mu.Lock()
	changed := s.Status != status
	s.Status = status
	hub := s.hub
	s.mu.Unlock()
	if changed && hub != nil {
		hub.notifySessions()
	}
}

// snapshot returns a JSON-safe copy of session state.
func (s *serverSession) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"id":                s.ID,
		"name":              s.Name,
		"project":           s.Project,
		"model":             s.Model,
		"status":            s.Status,
		"created_at":        s.CreatedAt.Format(time.RFC3339),
		"last_active":       s.LastActive.Format(time.RFC3339),
		"total_cost_usd":    s.TotalCost,
		"num_turns":         s.NumTurns,
		"claude_session_id": s.ClaudeSID,
		"stream_url":        s.StreamURL,
		"soul_enabled":      s.SoulEnabled,
		"subscribers":       s.broadcaster.count(),
	}
}

// ── Session Manager ──

// sessionManager manages concurrent sessions with TTL.
type sessionManager struct {
	sessions    map[string]*serverSession
	maxSessions int
	idleTimeout time.Duration
	maxLifetime time.Duration
	mu          sync.RWMutex
	stopReaper  chan struct{}
	hub         *wsHub // WebSocket hub for real-time notifications
}

// newSessionManager creates a session manager and starts the TTL reaper.
func newSessionManager(maxSessions int, idleTimeout, maxLifetime time.Duration) *sessionManager {
	sm := &sessionManager{
		sessions:    make(map[string]*serverSession),
		maxSessions: maxSessions,
		idleTimeout: idleTimeout,
		maxLifetime: maxLifetime,
		stopReaper:  make(chan struct{}),
	}
	go sm.reaper()
	return sm
}

// createSession spawns a new Claude Code session.
func (sm *sessionManager) createSession(name, project, model string, soulEnabled bool, mcpConfig string) (*serverSession, error) {
	sm.mu.Lock()
	if len(sm.sessions) >= sm.maxSessions {
		sm.mu.Unlock()
		return nil, fmt.Errorf("max sessions reached (%d)", sm.maxSessions)
	}
	sm.mu.Unlock()

	id := uuid.New().String()
	now := time.Now()

	bc := newBroadcaster()
	bc.hub = sm.hub
	bc.sessionID = id

	sess := &serverSession{
		ID:          id,
		Name:        name,
		Project:     project,
		Model:       model,
		Status:      "starting",
		CreatedAt:   now,
		LastActive:  now,
		SoulEnabled: soulEnabled,
		StreamURL:   fmt.Sprintf("/api/sessions/%s/stream", id),
		broadcaster: bc,
		hub:         sm.hub,
	}

	// Build soul prompt if enabled
	var promptFile string
	if soulEnabled {
		initSessionDir()
		result := buildPrompt()
		writePrompt(result)
		promptFile = promptOut
		sess.promptFile = promptFile
	}

	// Spawn Claude Code process
	opts := sessionOpts{
		WorkDir:          project,
		SystemPromptFile: promptFile,
		Model:            model,
		MCPConfig:        mcpConfig,
	}
	proc, err := spawnClaude(opts)
	if err != nil {
		return nil, fmt.Errorf("spawn claude: %w", err)
	}
	sess.process = proc
	sess.setStatus("running")

	// Register in server DB
	ensureServerSession(id, name)

	// Start stdout → SSE/WS bridge
	go bridgeStdout(proc, sess.broadcaster, func(raw json.RawMessage) {
		var result ResultMessage
		if json.Unmarshal(raw, &result) == nil {
			sess.mu.Lock()
			sess.TotalCost += result.TotalCostUSD
			sess.NumTurns += result.NumTurns
			if result.SessionID != "" {
				sess.ClaudeSID = result.SessionID
			}
			newStatus := "idle"
			if result.Subtype == "error" {
				newStatus = "error"
			}
			sess.mu.Unlock()
			sess.setStatus(newStatus) // notifies WS hub

			// Track user turns for auto-rename (result = end of a turn)
			if result.NumTurns > 0 {
				turns, renamed := incrementUserTurns(sess.ID)
				if !renamed && turns > 0 && turns%5 == 0 && !isAutoNamed(sess.ID) {
					go tryAutoRename(sess)
				}
			}
		}
		sess.touch()
	})

	// Watch for process exit
	go func() {
		<-proc.done
		sess.mu.Lock()
		alreadyStopped := sess.Status == "stopped"
		sess.mu.Unlock()
		if alreadyStopped {
			return
		}
		if proc.exitCode != 0 {
			sess.setStatus("error")
		} else {
			sess.setStatus("stopped")
		}
	}()

	// Drain stderr in background (prevent pipe deadlock)
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := proc.stderr.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	sm.mu.Lock()
	sm.sessions[id] = sess
	sm.mu.Unlock()

	return sess, nil
}

// getSession returns a session by ID.
func (sm *sessionManager) getSession(id string) *serverSession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[id]
}

// listSessions returns snapshots of all active sessions.
func (sm *sessionManager) listSessions() []map[string]any {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	list := make([]map[string]any, 0, len(sm.sessions))
	for _, sess := range sm.sessions {
		list = append(list, sess.snapshot())
	}
	return list
}

// destroySession gracefully shuts down and removes a session.
func (sm *sessionManager) destroySession(id string) error {
	sm.mu.Lock()
	sess, ok := sm.sessions[id]
	if !ok {
		sm.mu.Unlock()
		return fmt.Errorf("session not found: %s", id)
	}
	delete(sm.sessions, id)
	sm.mu.Unlock()

	sess.setStatus("stopped")
	if sess.process != nil && sess.process.alive() {
		sess.process.shutdown()
	}

	// Clean up temp prompt file
	if sess.promptFile != "" {
		os.Remove(sess.promptFile)
	}

	if sm.hub != nil {
		sm.hub.notifySessions()
	}
	return nil
}

// shutdownAll gracefully stops all sessions. Used on server shutdown.
func (sm *sessionManager) shutdownAll() {
	close(sm.stopReaper)

	sm.mu.Lock()
	ids := make([]string, 0, len(sm.sessions))
	for id := range sm.sessions {
		ids = append(ids, id)
	}
	sm.mu.Unlock()

	for _, id := range ids {
		sm.destroySession(id)
	}
}

// reaper runs every minute and destroys idle/expired sessions.
func (sm *sessionManager) reaper() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-sm.stopReaper:
			return
		case <-ticker.C:
			sm.reap()
		}
	}
}

func (sm *sessionManager) reap() {
	now := time.Now()

	sm.mu.RLock()
	var toDestroy []string
	for id, sess := range sm.sessions {
		sess.mu.Lock()
		idle := now.Sub(sess.LastActive) > sm.idleTimeout
		expired := now.Sub(sess.CreatedAt) > sm.maxLifetime
		dead := sess.process != nil && !sess.process.alive()
		sess.mu.Unlock()

		if idle || expired || dead {
			toDestroy = append(toDestroy, id)
		}
	}
	sm.mu.RUnlock()

	for _, id := range toDestroy {
		fmt.Fprintf(os.Stderr, "[%s] server: reaping session %s\n", appName, shortID(id))
		sm.destroySession(id)
	}
}

// ── Resume Session ──

// resumeSession creates a new active session wrapping `claude --resume <sessionID>`.
func (sm *sessionManager) resumeSession(sessionID, message string) (*serverSession, error) {
	sm.mu.Lock()
	if len(sm.sessions) >= sm.maxSessions {
		sm.mu.Unlock()
		return nil, fmt.Errorf("max sessions reached (%d)", sm.maxSessions)
	}
	sm.mu.Unlock()

	id := uuid.New().String()
	now := time.Now()

	bc := newBroadcaster()
	bc.hub = sm.hub
	bc.sessionID = id

	sess := &serverSession{
		ID:          id,
		Name:        "resume-" + shortID(sessionID),
		Project:     workspace,
		Status:      "starting",
		CreatedAt:   now,
		LastActive:  now,
		SoulEnabled: true,
		StreamURL:   fmt.Sprintf("/api/sessions/%s/stream", id),
		broadcaster: bc,
		hub:         sm.hub,
	}

	// Build soul prompt
	initSessionDir()
	result := buildPrompt()
	writePrompt(result)
	sess.promptFile = promptOut

	// Spawn Claude Code with --resume
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
		"--append-system-prompt-file", promptOut,
		"--resume", sessionID,
	}

	cmd := exec.Command(claudeBin, args...)
	cmd.Dir = workspace
	env := filterEnv(os.Environ(), "CLAUDECODE")
	env = append(env, "CLAUDE_CODE_ENTRYPOINT=sdk-go")
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}

	proc := &claudeProcess{
		cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr,
		done: make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				proc.exitCode = exitErr.ExitCode()
			} else {
				proc.exitCode = 1
			}
		}
		close(proc.done)
	}()

	sess.process = proc
	sess.setStatus("running")

	// Register in server DB
	ensureServerSession(id, sess.Name)

	go bridgeStdout(proc, sess.broadcaster, func(raw json.RawMessage) {
		var res ResultMessage
		if json.Unmarshal(raw, &res) == nil {
			sess.mu.Lock()
			sess.TotalCost += res.TotalCostUSD
			sess.NumTurns += res.NumTurns
			if res.SessionID != "" {
				sess.ClaudeSID = res.SessionID
			}
			newStatus := "idle"
			if res.Subtype == "error" {
				newStatus = "error"
			}
			sess.mu.Unlock()
			sess.setStatus(newStatus)

			// Track user turns for auto-rename
			if res.NumTurns > 0 {
				turns, renamed := incrementUserTurns(sess.ID)
				if !renamed && turns > 0 && turns%5 == 0 && !isAutoNamed(sess.ID) {
					go tryAutoRename(sess)
				}
			}
		}
		sess.touch()
	})

	go func() {
		<-proc.done
		sess.mu.Lock()
		alreadyStopped := sess.Status == "stopped"
		sess.mu.Unlock()
		if alreadyStopped {
			return
		}
		if proc.exitCode != 0 {
			sess.setStatus("error")
		} else {
			sess.setStatus("stopped")
		}
	}()

	// Drain stderr
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := proc.stderr.Read(buf); err != nil {
				return
			}
		}
	}()

	sm.mu.Lock()
	sm.sessions[id] = sess
	sm.mu.Unlock()

	// Send initial message if provided
	if message != "" {
		time.Sleep(500 * time.Millisecond)
		proc.sendMessage(message)
	}

	fmt.Fprintf(os.Stderr, "[%s] server: resumed session %s as %s\n", appName, shortID(sessionID), shortID(id))
	return sess, nil
}

// ── JSONL History Parsing ──

// findSessionJSONL locates the JSONL file for a given session ID.
func findSessionJSONL(sessionID string) string {
	claudeProjects := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(claudeProjects)
	if err != nil {
		return ""
	}
	fname := sessionID + ".jsonl"
	for _, projEntry := range entries {
		if !projEntry.IsDir() {
			continue
		}
		path := filepath.Join(claudeProjects, projEntry.Name(), fname)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// historyMessage is a parsed message from a session JSONL for the UI.
type historyMessage struct {
	Role      string `json:"role"`                 // user, assistant, system, tool_use, tool_result
	Content   string `json:"content"`              // text content
	ToolName  string `json:"tool_name,omitempty"`   // for tool_use
	ToolInput string `json:"tool_input,omitempty"`  // for tool_use
	Timestamp string `json:"timestamp,omitempty"`
}

// parseSessionMessages reads a JSONL file and extracts the last N messages for display.
func parseSessionMessages(path string, limit int) []historyMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var all []historyMessage

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}

		var ev struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Model   string          `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}

		switch ev.Type {
		case "user":
			text := extractContentText(ev.Message.Content)
			if text != "" {
				all = append(all, historyMessage{
					Role:      "user",
					Content:   text,
					Timestamp: ev.Timestamp,
				})
			}

		case "assistant":
			var blocks []struct {
				Type    string          `json:"type"`
				Text    string          `json:"text"`
				Name    string          `json:"name"`
				Input   json.RawMessage `json:"input"`
			}
			if json.Unmarshal(ev.Message.Content, &blocks) != nil {
				continue
			}

			var textParts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if b.Text != "" {
						textParts = append(textParts, b.Text)
					}
				case "tool_use":
					// Flush any accumulated text first
					if len(textParts) > 0 {
						all = append(all, historyMessage{
							Role:      "assistant",
							Content:   strings.Join(textParts, "\n"),
							Timestamp: ev.Timestamp,
						})
						textParts = nil
					}
					inputStr := string(b.Input)
					if len(inputStr) > 500 {
						inputStr = inputStr[:500] + "..."
					}
					all = append(all, historyMessage{
						Role:      "tool_use",
						ToolName:  b.Name,
						ToolInput: inputStr,
						Timestamp: ev.Timestamp,
					})
				case "tool_result":
					resultText := extractContentText(b.Input)
					if len(resultText) > 500 {
						resultText = resultText[:500] + "..."
					}
					all = append(all, historyMessage{
						Role:    "tool_result",
						Content: resultText,
					})
				}
			}
			if len(textParts) > 0 {
				all = append(all, historyMessage{
					Role:      "assistant",
					Content:   strings.Join(textParts, "\n"),
					Timestamp: ev.Timestamp,
				})
			}
		}
	}

	// Return only last N messages
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all
}

// extractContentText extracts text from a JSON content field (string or array of blocks).
func extractContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try string first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}
