package main

import (
	"encoding/json"
	"fmt"
	"os"
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
	mu          sync.Mutex
}

// touch updates LastActive timestamp.
func (s *serverSession) touch() {
	s.mu.Lock()
	s.LastActive = time.Now()
	s.mu.Unlock()
}

// setStatus atomically updates session status.
func (s *serverSession) setStatus(status string) {
	s.mu.Lock()
	s.Status = status
	s.mu.Unlock()
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
		broadcaster: newBroadcaster(),
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

	// Start stdout → SSE bridge
	go bridgeStdout(proc, sess.broadcaster, func(raw json.RawMessage) {
		// Parse result message to update session stats
		var result ResultMessage
		if json.Unmarshal(raw, &result) == nil {
			sess.mu.Lock()
			sess.TotalCost += result.TotalCostUSD
			sess.NumTurns += result.NumTurns
			if result.Subtype == "error" {
				sess.Status = "error"
			} else {
				sess.Status = "idle"
			}
			if result.SessionID != "" {
				sess.ClaudeSID = result.SessionID
			}
			sess.mu.Unlock()
		}
		sess.touch()
	})

	// Watch for process exit
	go func() {
		<-proc.done
		sess.mu.Lock()
		if sess.Status != "stopped" {
			if proc.exitCode != 0 {
				sess.Status = "error"
			} else {
				sess.Status = "stopped"
			}
		}
		sess.mu.Unlock()
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
