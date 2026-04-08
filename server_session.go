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

// Session categories determine lifecycle behavior.
const (
	CategoryInteractive = "interactive" // Web UI manual sessions — subject to maxSessions, normal TTL
	CategoryHeartbeat   = "heartbeat"   // Wake/heartbeat — auto-destroy on completion, not counted toward maxSessions
	CategoryCron        = "cron"        // Scheduled tasks — auto-destroy on completion
	CategoryEvolve      = "evolve"      // Daily self-evolution — auto-destroy on completion
)

// isEphemeralCategory returns true if sessions in this category should auto-destroy when done.
func isEphemeralCategory(cat string) bool {
	return cat == CategoryHeartbeat || cat == CategoryCron || cat == CategoryEvolve
}

// serverSession represents one active Claude Code session.
type serverSession struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Project       string    `json:"project"`
	Model         string    `json:"model,omitempty"`
	Status        string    `json:"status"` // starting, running, idle, stopped, error
	Category      string    `json:"category"`            // interactive, heartbeat, cron, evolve
	Tags          []string  `json:"tags,omitempty"`       // freeform labels for filtering
	CreatedAt     time.Time `json:"created_at"`
	LastActive    time.Time `json:"last_active"`
	TotalCost     float64   `json:"total_cost_usd"`
	NumTurns      int       `json:"num_turns"`
	ClaudeSID     string    `json:"claude_session_id,omitempty"` // Claude Code's own session ID
	ResumedFrom   string    `json:"resumed_from,omitempty"`      // Original session ID being resumed
	StreamURL     string    `json:"stream_url"`
	SoulEnabled   bool      `json:"soul_enabled"`
	ChromeEnabled bool      `json:"chrome_enabled"`
	GalID         string    `json:"gal_id,omitempty"` // GAL save id this session was resumed from

	process     *claudeProcess
	broadcaster *sseBroadcaster
	promptFile  string // temp file for soul prompt
	mcpConfig   string // remembered for reload
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
	galID := s.GalID
	if galID == "" {
		galID = getGalID(s.ID)
	}
	tags := s.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"id":                s.ID,
		"name":              s.Name,
		"project":           s.Project,
		"model":             s.Model,
		"status":            s.Status,
		"category":          s.Category,
		"tags":              tags,
		"created_at":        s.CreatedAt.Format(time.RFC3339),
		"last_active":       s.LastActive.Format(time.RFC3339),
		"total_cost_usd":    s.TotalCost,
		"num_turns":         s.NumTurns,
		"claude_session_id": s.ClaudeSID,
		"resumed_from":      s.ResumedFrom,
		"stream_url":        s.StreamURL,
		"agent":             "main",
		"soul_enabled":      s.SoulEnabled,
		"chrome_enabled":    s.ChromeEnabled,
		"gal_id":            galID,
		"subscribers":       s.broadcaster.count(),
		"last_event":        s.broadcaster.lastEventAt(),
		"idle_seconds":      s.broadcaster.idleSeconds(),
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

// sessionCreateOpts holds options for creating a new session.
type sessionCreateOpts struct {
	Name        string
	Project     string
	Model       string
	Soul        bool
	MCP         string
	GalID       string
	Category    string   // interactive, heartbeat, cron, evolve, telegram (default: interactive)
	Tags        []string // freeform labels
	EnvOverride string   // if set, replaces the environment section in BOOT.md
}

// createSession spawns a new Claude Code session.
func (sm *sessionManager) createSession(name, project, model string, soulEnabled bool, mcpConfig string, galID string) (*serverSession, error) {
	return sm.createSessionWithOpts(sessionCreateOpts{
		Name:     name,
		Project:  project,
		Model:    model,
		Soul:     soulEnabled,
		MCP:      mcpConfig,
		GalID:    galID,
		Category: CategoryInteractive,
	})
}

// createSessionWithOpts spawns a new Claude Code session with full options.
func (sm *sessionManager) createSessionWithOpts(opts sessionCreateOpts) (*serverSession, error) {
	category := opts.Category
	if category == "" {
		category = CategoryInteractive
	}

	// Only count interactive sessions toward maxSessions limit
	// Telegram, heartbeat, cron, evolve sessions bypass this limit.
	sm.mu.Lock()
	if category == CategoryInteractive {
		interactiveCount := 0
		for _, s := range sm.sessions {
			if s.Category == CategoryInteractive {
				interactiveCount++
			}
		}
		if interactiveCount >= sm.maxSessions {
			sm.mu.Unlock()
			return nil, fmt.Errorf("max interactive sessions reached (%d)", sm.maxSessions)
		}
	}
	sm.mu.Unlock()

	id := uuid.New().String()
	now := time.Now()

	bc := newBroadcaster()
	bc.hub = sm.hub
	bc.sessionID = id

	tags := opts.Tags
	if tags == nil {
		tags = []string{}
	}

	sess := &serverSession{
		ID:          id,
		Name:        opts.Name,
		Project:     opts.Project,
		Model:       opts.Model,
		Status:      "starting",
		Category:    category,
		Tags:        tags,
		CreatedAt:   now,
		LastActive:  now,
		SoulEnabled: opts.Soul,
		StreamURL:   fmt.Sprintf("/api/sessions/%s/stream", id),
		mcpConfig:   opts.MCP,
		broadcaster: bc,
		hub:         sm.hub,
		GalID:       opts.GalID,
	}

	// Load GAL save for resume if provided
	if opts.GalID != "" {
		galPath := filepath.Join(workspace, "memory", "gal", opts.GalID+".json")
		if data, err := os.ReadFile(galPath); err == nil {
			galContext = string(data)
		}
	}

	// Build soul prompt if enabled
	var promptFile string
	if opts.Soul {
		initSessionDir()
		// Only inject HEARTBEAT.md for heartbeat/cron sessions
		includeHeartbeat = opts.Category == CategoryHeartbeat || opts.Category == CategoryCron
		// Allow per-session environment override (e.g. Telegram mode)
		if opts.EnvOverride != "" {
			sessionEnvOverride = opts.EnvOverride
		}
		result := buildPrompt()
		sessionEnvOverride = "" // reset after use
		writePrompt(result)
		promptFile = promptOut
		sess.promptFile = promptFile
	}

	// Spawn Claude Code process
	spawnOpts := sessionOpts{
		WorkDir:          opts.Project,
		SystemPromptFile: promptFile,
		Model:            opts.Model,
		MCPConfig:        opts.MCP,
	}
	proc, err := spawnClaude(spawnOpts)
	if err != nil {
		return nil, fmt.Errorf("spawn claude: %w", err)
	}
	sess.process = proc
	sess.setStatus("running")

	// Register in server DB with category and tags
	ensureServerSessionFull(id, opts.Name, category, opts.Tags)
	if opts.GalID != "" {
		setGalID(id, opts.GalID)
	}

	// Start stdout → SSE/WS bridge
	go bridgeStdout(proc, sess.broadcaster,
		// onInit: sync session name from Claude Code metadata
		func(raw json.RawMessage) {
			var init InitMessage
			if json.Unmarshal(raw, &init) == nil && init.SessionID != "" {
				sess.mu.Lock()
				sess.ClaudeSID = init.SessionID
				sess.mu.Unlock()
				// Record Claude session ID → weiran session mapping for history lookup
				setClaudeSessionID(sess.ID, init.SessionID)
				// Record agent identity for this Claude session
				recordSessionAgent(init.SessionID, "main", appName, "server-create")
				// Read Claude Code's session name (slug like "distributed-munching-kitten")
				if ccName := readClaudeSessionName(init.SessionID); ccName != "" {
					sess.mu.Lock()
					if sess.Name == "" || strings.HasPrefix(sess.Name, "session-") || strings.HasPrefix(sess.Name, "resume-") {
						sess.Name = ccName
					}
					sess.mu.Unlock()
					ensureServerSession(sess.ID, ccName)
					if sess.hub != nil {
						sess.hub.notifySessions()
					}
				}
			}
		},
		// onResult
		func(raw json.RawMessage) {
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
				sess.setStatus(newStatus)

				// Sync session name from Claude Code metadata.
				// CC may rename sessions multiple times as conversation evolves,
				// so keep syncing as long as the user hasn't manually renamed.
				sess.mu.Lock()
				claudeSID := sess.ClaudeSID
				currentName := sess.Name
				sess.mu.Unlock()
				if claudeSID != "" && !isManuallyRenamed(sess.ID) {
					if ccName := readClaudeSessionName(claudeSID); ccName != "" && ccName != currentName {
						sess.mu.Lock()
						sess.Name = ccName
						sess.mu.Unlock()
						markAutoNamed(sess.ID, ccName)
						if sess.hub != nil {
							sess.hub.notifySessions()
						}
					}
				}

				// Track user turns for auto-rename (fallback if CC doesn't name it)
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

// setChrome reloads a session's underlying claude process with --chrome
// toggled on/off. The serverSession (broadcaster, history, name, etc.) is
// preserved across the reload — only the subprocess is swapped.
func (sm *sessionManager) setChrome(id string, enabled bool) error {
	sess := sm.getSession(id)
	if sess == nil {
		return fmt.Errorf("session not found: %s", id)
	}

	sess.mu.Lock()
	if sess.ChromeEnabled == enabled && sess.process != nil && sess.process.alive() {
		sess.mu.Unlock()
		return nil // no-op
	}
	oldProc := sess.process
	opts := sessionOpts{
		WorkDir:          sess.Project,
		SystemPromptFile: sess.promptFile,
		Model:            sess.Model,
		MCPConfig:        sess.mcpConfig,
		Chrome:           enabled,
	}
	sess.mu.Unlock()

	// Mark the old process so its bridgeStdout doesn't broadcast a "close"
	// SSE event when it exits — this is an intentional reload.
	if oldProc != nil {
		oldProc.suppressClose.Store(true)
		if oldProc.alive() {
			oldProc.shutdown()
		}
	}

	sess.setStatus("starting")

	// Spawn replacement
	newProc, err := spawnClaude(opts)
	if err != nil {
		sess.setStatus("error")
		return fmt.Errorf("respawn claude: %w", err)
	}

	sess.mu.Lock()
	sess.process = newProc
	sess.ChromeEnabled = enabled
	// Reset Claude session ID — the new subprocess will report its own.
	sess.ClaudeSID = ""
	sess.mu.Unlock()

	setChromeEnabled(id, enabled)
	sess.setStatus("running")

	// Re-attach stdout bridge with the same callbacks as createSession.
	go bridgeStdout(newProc, sess.broadcaster,
		func(raw json.RawMessage) {
			var init InitMessage
			if json.Unmarshal(raw, &init) == nil && init.SessionID != "" {
				sess.mu.Lock()
				sess.ClaudeSID = init.SessionID
				sess.mu.Unlock()
			}
		},
		func(raw json.RawMessage) {
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
				sess.setStatus(newStatus)
			}
			sess.touch()
		})

	// Watch for process exit
	go func() {
		<-newProc.done
		sess.mu.Lock()
		alreadyStopped := sess.Status == "stopped"
		sess.mu.Unlock()
		if alreadyStopped {
			return
		}
		if newProc.exitCode != 0 {
			sess.setStatus("error")
		} else {
			sess.setStatus("stopped")
		}
	}()

	// Drain stderr
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := newProc.stderr.Read(buf); err != nil {
				return
			}
		}
	}()

	if sm.hub != nil {
		sm.hub.notifySessions()
	}
	fmt.Fprintf(os.Stderr, "[%s] server: session %s chrome=%v reloaded\n", appName, shortID(id), enabled)
	return nil
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

	// Clean up persistent per-session state (chrome flag, rename meta, etc.)
	clearSessionRow(id)

	// Clear telegram chat→session mapping if this was a TG session
	if sess.Category == CategoryTelegram {
		clearTGSessionMapping(id)
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
		category := sess.Category
		sess.mu.Unlock()

		// Ephemeral sessions (heartbeat/cron/evolve): destroy as soon as process exits
		if isEphemeralCategory(category) && dead {
			toDestroy = append(toDestroy, id)
			continue
		}

		// Telegram sessions: destroy on idle/expired but not on dead (will be recreated on next message)
		if category == CategoryTelegram {
			if idle || expired {
				toDestroy = append(toDestroy, id)
			}
			continue
		}

		// Interactive sessions: normal TTL rules
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
// If a session resuming the same Claude session ID is already active, return it.
func (sm *sessionManager) resumeSession(sessionID, message, displayName, categoryOverride string) (*serverSession, error) {
	sm.mu.Lock()
	// Dedup: check if already resumed (by ClaudeSID or ResumedFrom), only if alive
	for _, s := range sm.sessions {
		s.mu.Lock()
		cid := s.ClaudeSID
		rfrom := s.ResumedFrom
		alive := s.process != nil && s.process.alive()
		s.mu.Unlock()
		if alive && (cid == sessionID || rfrom == sessionID) {
			sm.mu.Unlock()
			return s, nil
		}
	}
	// Only count interactive sessions toward maxSessions
	interactiveCount := 0
	for _, s := range sm.sessions {
		if s.Category == CategoryInteractive {
			interactiveCount++
		}
	}
	if interactiveCount >= sm.maxSessions {
		sm.mu.Unlock()
		return nil, fmt.Errorf("max interactive sessions reached (%d)", sm.maxSessions)
	}
	sm.mu.Unlock()

	id := uuid.New().String()
	now := time.Now()

	if displayName == "" {
		displayName = "resume-" + shortID(sessionID)
	}

	bc := newBroadcaster()
	bc.hub = sm.hub
	bc.sessionID = id

	// Category priority: explicit override > inherited from original session > default
	resolvedCategory := categoryOverride
	if resolvedCategory == "" {
		resolvedCategory = getSessionCategory(sessionID)
	}
	if resolvedCategory == "" {
		resolvedCategory = CategoryInteractive
	}

	sess := &serverSession{
		ID:          id,
		Name:        displayName,
		ResumedFrom: sessionID,
		Project:     workspace,
		Status:      "starting",
		Category:    resolvedCategory,
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
	env = injectProxyEnv(env)
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

	go bridgeStdout(proc, sess.broadcaster,
		// onInit: sync session name
		func(raw json.RawMessage) {
			var init InitMessage
			if json.Unmarshal(raw, &init) == nil && init.SessionID != "" {
				sess.mu.Lock()
				sess.ClaudeSID = init.SessionID
				sess.mu.Unlock()
				// Record agent identity (resume inherits from original)
				recordSessionAgent(init.SessionID, "main", appName, "server-resume")
				if ccName := readClaudeSessionName(init.SessionID); ccName != "" {
					sess.mu.Lock()
					if sess.Name == "" || strings.HasPrefix(sess.Name, "session-") || strings.HasPrefix(sess.Name, "resume-") {
						sess.Name = ccName
					}
					sess.mu.Unlock()
					ensureServerSession(sess.ID, ccName)
					if sess.hub != nil {
						sess.hub.notifySessions()
					}
				}
			}
		},
		// onResult
		func(raw json.RawMessage) {
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

				// Sync session name from Claude Code metadata as long as the
				// user hasn't manually renamed the session.
				sess.mu.Lock()
				claudeSID := sess.ClaudeSID
				currentName := sess.Name
				sess.mu.Unlock()
				if claudeSID != "" && !isManuallyRenamed(sess.ID) {
					if ccName := readClaudeSessionName(claudeSID); ccName != "" && ccName != currentName {
						sess.mu.Lock()
						sess.Name = ccName
						sess.mu.Unlock()
						markAutoNamed(sess.ID, ccName)
						if sess.hub != nil {
							sess.hub.notifySessions()
						}
					}
				}

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
		userEvent, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": message},
		})
		sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})
		proc.sendMessage(message)
	}

	fmt.Fprintf(os.Stderr, "[%s] server: resumed session %s as %s\n", appName, shortID(sessionID), shortID(id))
	return sess, nil
}

// readClaudeSessionName reads the session name from Claude Code's session meta file.
func readClaudeSessionName(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	// Claude Code stores session meta as numeric-ID .json files,
	// each containing a "sessionId" (UUID) and optional "name".
	sessDir := filepath.Join(home, ".claude", "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sessDir, e.Name()))
		if err != nil {
			continue
		}
		var meta struct {
			SessionID string `json:"sessionId"`
			Name      string `json:"name"`
		}
		if json.Unmarshal(data, &meta) == nil && meta.SessionID == sessionID && meta.Name != "" {
			return meta.Name
		}
	}
	return ""
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
	Role      string         `json:"role"`                 // user, assistant, system, tool_use, tool_result, image
	Content   string         `json:"content"`              // text content
	ToolName  string         `json:"tool_name,omitempty"`  // for tool_use
	ToolInput string         `json:"tool_input,omitempty"` // for tool_use
	Timestamp string         `json:"timestamp,omitempty"`
	Images    []historyImage `json:"images,omitempty"` // base64 images from tool results
}

type historyImage struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
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
			// Check for images in tool_result content blocks
			images := extractImages(ev.Message.Content)
			if len(images) > 0 {
				all = append(all, historyMessage{
					Role:      "image",
					Images:    images,
					Timestamp: ev.Timestamp,
				})
			}
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
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
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

// extractImages finds base64 image blocks in user message content (tool_result responses).
func extractImages(raw json.RawMessage) []historyImage {
	if len(raw) == 0 {
		return nil
	}
	// content is an array of blocks like [{type:"tool_result", content:[{type:"image", source:{...}}]}]
	var blocks []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var images []historyImage
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		// content can be string or array
		var items []struct {
			Type   string `json:"type"`
			Source struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source"`
		}
		if json.Unmarshal(b.Content, &items) != nil {
			continue
		}
		for _, item := range items {
			if item.Type == "image" && item.Source.Type == "base64" && item.Source.Data != "" {
				images = append(images, historyImage{
					MediaType: item.Source.MediaType,
					Data:      item.Source.Data,
				})
			}
		}
	}
	return images
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
