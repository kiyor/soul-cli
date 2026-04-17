package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// expandHome expands a leading ~ to the user's home directory.
func expandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") && path != "~" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path, fmt.Errorf("cannot determine home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

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
	ReplaceSoul   bool      `json:"replace_soul"`     // 本我模式 — use --system-prompt-file (replace) instead of --append-system-prompt-file
	FirstMsg      string          `json:"first_msg,omitempty"` // First user message (for hint display)
	GalID         string          `json:"gal_id,omitempty"`    // GAL save id this session was resumed from
	Todos         json.RawMessage `json:"todos,omitempty"`     // latest TodoWrite state (broadcast to all clients)
	SpawnedBy     string          `json:"spawned_by,omitempty"` // parent session ID that spawned this one

	process     *claudeProcess
	broadcaster *sseBroadcaster
	bridgeDone  chan struct{} // closed when the current bridgeStdout goroutine exits
	promptFile  string        // temp file for soul prompt
	mcpConfig   string        // remembered for reload
	hub         *wsHub        // for WS notifications on status change
	waiters     []chan string  // notified when status becomes idle/stopped/error

	// pendingAUQ tracks in-flight AskUserQuestion permission requests that the
	// Web UI is rendering. Keyed by the control_request.request_id. The claude
	// subprocess blocks waiting for a control_response (with the user's answers
	// in updatedInput) until the /answer-question endpoint fires.
	pendingAUQMu sync.Mutex
	pendingAUQ   map[string]*pendingAUQEntry

	// Model fallback for non-interactive modes (heartbeat/cron/evolve):
	// when process exits with rate_limit, retry with next fallback model.
	fallbackModels []string // remaining models to try
	taskMessage    string   // original task message for retry
	sessionMgr     *sessionManager // back-reference for fallback retry

	mu          sync.Mutex
}

// pendingAUQEntry remembers enough about an in-flight AskUserQuestion
// control_request to answer it later with the user's choices.
type pendingAUQEntry struct {
	RequestID string          // control_request.request_id from claude
	ToolUseID string          // tool_use_id from the can_use_tool payload (for UI dedupe)
	Input     json.RawMessage // original input — must be echoed back in updatedInput
	CreatedAt time.Time
}

// recordPendingAUQ stores a pending AskUserQuestion by request_id.
func (s *serverSession) recordPendingAUQ(entry *pendingAUQEntry) {
	s.pendingAUQMu.Lock()
	defer s.pendingAUQMu.Unlock()
	if s.pendingAUQ == nil {
		s.pendingAUQ = make(map[string]*pendingAUQEntry)
	}
	s.pendingAUQ[entry.RequestID] = entry
}

// takePendingAUQ atomically removes and returns a pending AUQ entry.
// Returns nil if no entry exists for the given request_id.
func (s *serverSession) takePendingAUQ(requestID string) *pendingAUQEntry {
	s.pendingAUQMu.Lock()
	defer s.pendingAUQMu.Unlock()
	entry, ok := s.pendingAUQ[requestID]
	if !ok {
		return nil
	}
	delete(s.pendingAUQ, requestID)
	return entry
}

// touch updates LastActive timestamp.
func (s *serverSession) touch() {
	s.mu.Lock()
	s.LastActive = time.Now()
	s.mu.Unlock()
}

// setTodos stores the latest TodoWrite state and broadcasts session list update.
func (s *serverSession) setTodos(todos json.RawMessage) {
	s.mu.Lock()
	s.Todos = todos
	hub := s.hub
	s.mu.Unlock()
	if hub != nil {
		hub.notifySessions()
	}
}

// setStatus atomically updates session status and notifies WS clients.
func (s *serverSession) setStatus(status string) {
	s.mu.Lock()
	changed := s.Status != status
	s.Status = status
	id := s.ID
	hub := s.hub
	// Notify waiters when session reaches a terminal/current state
	var waitersToNotify []chan string
	if changed && (status == "idle" || status == "stopped" || status == "error") {
		waitersToNotify = s.waiters
		s.waiters = nil
	}
	s.mu.Unlock()
	// Fire waiter notifications outside the lock
	for _, ch := range waitersToNotify {
		ch <- status
	}
	if changed {
		// Persist proxy-aggregated cost when session ends
		if status == "stopped" || status == "error" {
			go persistSessionCost(id)
		}
		if hub != nil {
			hub.notifySessions()
		}
	}
}

// snapshot returns a JSON-safe copy of session state.
// NOTE: To avoid deadlocks, DB queries and broadcaster calls are done OUTSIDE
// sess.mu. Lock ordering: sess.mu must never be held when acquiring broadcaster.mu
// (broadcast() holds broadcaster.mu → calls hub → calls listSessions → snapshot).
func (s *serverSession) snapshot() map[string]any {
	// 1. Read broadcaster metrics BEFORE acquiring sess.mu (avoids sess.mu → broadcaster.mu inversion)
	subCount := s.broadcaster.count()
	lastEvent := s.broadcaster.lastEventAt()
	idleSecs := s.broadcaster.idleSeconds()

	// 2. Copy session fields under lock
	s.mu.Lock()
	id := s.ID
	galID := s.GalID
	tags := s.Tags
	if tags == nil {
		tags = []string{}
	}
	var todosSnap json.RawMessage
	if len(s.Todos) > 0 {
		todosSnap = make(json.RawMessage, len(s.Todos))
		copy(todosSnap, s.Todos)
	}
	spawnedBy := s.SpawnedBy
	snap := map[string]any{
		"id":                id,
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
		"replace_soul":      s.ReplaceSoul,
		"first_msg":         s.FirstMsg,
		"spawned_by":        spawnedBy,
	}
	if todosSnap != nil {
		snap["todos"] = todosSnap
	}
	s.mu.Unlock()

	// 3. DB queries and broadcaster values OUTSIDE lock (no deadlock risk)
	if galID == "" {
		galID = getGalID(id)
	}
	snap["gal_id"] = galID
	snap["proxy_cost_usd"] = getSessionProxyCost(id)
	snap["subscribers"] = subCount
	snap["last_event"] = lastEvent
	snap["idle_seconds"] = idleSecs
	snap["participants"] = getParticipants(id)

	return snap
}

// ── Session Lifecycle Helpers ──

// makeOnInit returns the onInit callback for bridgeStdout.
// source: "server-create", "server-resume" → full init (record agent, sync CC name).
// source: "" → reload init (only sync ClaudeSID/Model, notify hub).
func makeOnInit(sess *serverSession, source string) func(json.RawMessage) {
	return func(raw json.RawMessage) {
		var init InitMessage
		if json.Unmarshal(raw, &init) == nil && init.SessionID != "" {
			sess.mu.Lock()
			sess.ClaudeSID = init.SessionID
			if init.Model != "" {
				before := sess.Model
				sess.Model = mergeInitModel(sess.Model, init.Model)
				fmt.Fprintf(os.Stderr, "[%s] server: onInit model: before=%q init=%q after=%q (session %s)\n",
					appName, before, init.Model, sess.Model, shortID(sess.ID))
			}
			hub := sess.hub // read under lock to avoid race
			sess.mu.Unlock()

			if source != "" {
				// Full init: record mappings and sync CC name
				setClaudeSessionID(sess.ID, init.SessionID)
				recordSessionAgent(init.SessionID, "main", appName, source)
				if ccName := readClaudeSessionName(init.SessionID); ccName != "" {
					sess.mu.Lock()
					if sess.Name == "" || strings.HasPrefix(sess.Name, "session-") || strings.HasPrefix(sess.Name, "resume-") {
						sess.Name = ccName
					}
					sess.mu.Unlock()
					ensureServerSession(sess.ID, ccName)
				}
			}
			if hub != nil {
				hub.notifySessions()
			}
		}
	}
}

// makeOnResult returns the onResult callback for bridgeStdout.
// fullSync: true for create/resume (CC name sync + auto-rename tracking).
// fullSync: false for reload (cost/turns only).
func makeOnResult(sess *serverSession, fullSync bool) func(json.RawMessage) {
	return func(raw json.RawMessage) {
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
				// Detect 429/rate-limit in result text for ephemeral sessions.
				// Claude Code handles 429 internally and may exit cleanly (code 0),
				// so we must detect it here in stdout and flag the process for fallback.
				if isEphemeralCategory(sess.Category) && len(sess.fallbackModels) > 0 && sess.process != nil {
					lower := strings.ToLower(result.Result)
					if strings.Contains(lower, "429") || strings.Contains(lower, "rate limit") ||
						strings.Contains(lower, "too many requests") || strings.Contains(lower, "quota exceeded") {
						proc := sess.process
						sess.mu.Unlock()
						proc.rateLimited.Store(true)
						fmt.Fprintf(os.Stderr, "[%s] server: detected 429/rate-limit in result message, killing process for fallback\n", appName)
						proc.cmd.Process.Kill()
						return
					}
				}
			}
			sess.mu.Unlock()
			sess.setStatus(newStatus)

			if fullSync {
				// Sync session name from Claude Code metadata
				sess.mu.Lock()
				claudeSID := sess.ClaudeSID
				currentName := sess.Name
				hub := sess.hub // read under lock
				sess.mu.Unlock()
				if claudeSID != "" && !isManuallyRenamed(sess.ID) {
					if ccName := readClaudeSessionName(claudeSID); ccName != "" && ccName != currentName {
						sess.mu.Lock()
						sess.Name = ccName
						sess.mu.Unlock()
						markAutoNamed(sess.ID, ccName)
						if hub != nil {
							hub.notifySessions()
						}
					}
				}

				// Track user turns for auto-rename
				if result.NumTurns > 0 {
					_, _ = incrementUserTurns(sess.ID)
					// Auto-rename disabled 2026-04-16: removes the only automated
					// Anthropic API call (haiku pool) when user runs on GLM/MiniMax
					// defaults. Manual rename via POST /api/sessions/{id}/auto-rename
					// still works if the user explicitly wants it.
					// if !renamed && turns > 0 && turns%5 == 0 && !isAutoNamed(sess.ID) {
					//     go tryAutoRename(sess)
					// }
				}
			}
		}
		sess.touch()
	}
}

// watchExit monitors process exit and updates session status.
// For ephemeral sessions (heartbeat/cron/evolve), if the exit is classified as
// rate_limit and fallback models are available, automatically retries with the
// next fallback model.
func watchExit(proc *claudeProcess, sess *serverSession) {
	go func() {
		<-proc.done
		sess.mu.Lock()
		alreadyStopped := sess.Status == "stopped"
		category := sess.Category
		fallbacks := append([]string(nil), sess.fallbackModels...)
		taskMsg := sess.taskMessage
		sm := sess.sessionMgr
		model := sess.Model
		claudeSID := sess.ClaudeSID
		sess.mu.Unlock()
		if alreadyStopped {
			return
		}
		// Determine if this was a rate_limit exit (either from exit code classification
		// or from real-time stderr detection via the rateLimited flag).
		isRateLimit := false
		if isEphemeralCategory(category) && len(fallbacks) > 0 && sm != nil {
			if proc.rateLimited.Load() {
				isRateLimit = true
			} else if proc.exitCode != 0 {
				event := classifyExitEvent(proc.exitCode, "", proc.stderrTail.String())
				isRateLimit = (event == "rate_limit")
			}
		}

		if isRateLimit {
			nextModel := fallbacks[0]
			remaining := fallbacks[1:]
			fmt.Fprintf(os.Stderr, "[%s] server: %s session %s hit rate_limit on %s, falling back to %s\n",
				appName, category, shortID(sess.ID), model, nextModel)
			// Fire-and-forget retry with next fallback model
			go retryWithFallbackModel(sm, sess, nextModel, remaining, taskMsg, claudeSID)
			sess.setStatus("error")
			return
		}

		if proc.exitCode != 0 {
			sess.setStatus("error")
		} else {
			sess.setStatus("stopped")
		}
	}()
}

// retryWithFallbackModel creates a new ephemeral session with the next fallback model
// after the current one failed with rate_limit.
func retryWithFallbackModel(sm *sessionManager, oldSess *serverSession, model string, remaining []string, taskMsg string, claudeSID string) {
	oldSess.mu.Lock()
	category := oldSess.Category
	project := oldSess.Project
	soul := oldSess.SoulEnabled
	replaceSoul := oldSess.ReplaceSoul
	oldModel := oldSess.Model
	oldSess.mu.Unlock()

	sessName := fmt.Sprintf("%s-fallback-%s", category, time.Now().Format("0102-150405"))
	fmt.Fprintf(os.Stderr, "[%s] server: creating fallback session %s with model %s (%d remaining fallbacks)\n",
		appName, sessName, model, len(remaining))

	// Resume the original Claude session to preserve conversation context.
	// For heartbeat, prefer the soul session (long-lived); for cron/evolve,
	// resume the specific Claude session that hit rate_limit.
	var resumeID string
	if category == CategoryHeartbeat {
		resumeID = getActiveSoulSession(agentName)
	}
	if resumeID == "" && claudeSID != "" {
		resumeID = claudeSID
		fmt.Fprintf(os.Stderr, "[%s] server: fallback resuming original Claude session %s\n",
			appName, shortID(claudeSID))
	}

	sess, err := sm.createSessionWithOpts(sessionCreateOpts{
		Name:        sessName,
		Project:     project,
		Model:       model,
		Soul:        soul,
		Category:    category,
		Tags:        []string{"auto", "fallback"},
		ReplaceSoul: replaceSoul,
		ResumeID:    resumeID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server: fallback session creation failed: %v\n", appName, err)
		// Continue consuming remaining fallback chain instead of giving up
		if len(remaining) > 0 {
			next := remaining[0]
			fmt.Fprintf(os.Stderr, "[%s] server: skipping %s, trying next fallback: %s\n", appName, model, next)
			go retryWithFallbackModel(sm, oldSess, next, remaining[1:], taskMsg, claudeSID)
		} else {
			go sendTelegram(fmt.Sprintf("❌ %s all fallback models exhausted (last: %s): %v", category, model, err))
		}
		return
	}

	// Set remaining fallbacks on the new session
	sess.mu.Lock()
	sess.fallbackModels = remaining
	sess.taskMessage = taskMsg
	sess.sessionMgr = sm
	sess.mu.Unlock()

	// Send the original task message — only if we didn't resume an existing
	// Claude session (which already has the task in its conversation history).
	// Sending it again on a resumed session would cause duplicate execution.
	if taskMsg != "" && resumeID != "" {
		fmt.Fprintf(os.Stderr, "[%s] server: fallback resumed session %s, skipping taskMsg re-send (already in conversation history)\n",
			appName, shortID(resumeID))
	}
	if taskMsg != "" && resumeID == "" {
		go func() {
			if !sess.process.waitInit(30 * time.Second) {
				fmt.Fprintf(os.Stderr, "[%s] server: fallback session init timeout for %s\n", appName, shortID(sess.ID))
				return
			}
			if err := sess.process.sendMessage(taskMsg); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] server: fallback session failed to send task: %v\n", appName, err)
			}
		}()
	}

	// Notify via Telegram
	notifyMsg := fmt.Sprintf("⚠️ %s model rate-limited, retrying with fallback: %s", oldModel, model)
	go sendTelegram(notifyMsg)
}

// waitBridgeDone waits for the current bridgeStdout goroutine to exit (max 10s).
// Must be called AFTER shutting down the old process but BEFORE starting a new bridge.
func (s *serverSession) waitBridgeDone() {
	s.mu.Lock()
	done := s.bridgeDone
	s.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Fprintf(os.Stderr, "[%s] server: bridge drain timeout for %s\n", appName, shortID(s.ID))
	}
}

// drainStderr reads stderr to prevent pipe deadlock, capturing the last 4KB
// in proc.stderrTail for exit event classification (e.g. rate_limit detection).
// For ephemeral sessions with fallback models, it also detects 429/rate-limit
// errors in real time and kills the process to trigger model fallback via watchExit.
func drainStderr(proc *claudeProcess, sess *serverSession) {
	// Determine if we should monitor for rate-limit errors
	sess.mu.Lock()
	shouldMonitor := isEphemeralCategory(sess.Category) && len(sess.fallbackModels) > 0
	sess.mu.Unlock()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := proc.stderr.Read(buf)
			if n > 0 {
				proc.stderrTail.Write(buf[:n])

				// Real-time 429 detection for ephemeral sessions
				if shouldMonitor && !proc.rateLimited.Load() {
					chunk := strings.ToLower(string(buf[:n]))
					if strings.Contains(chunk, "429") || strings.Contains(chunk, "rate limit") ||
						strings.Contains(chunk, "too many requests") || strings.Contains(chunk, "quota exceeded") {
						proc.rateLimited.Store(true)
						fmt.Fprintf(os.Stderr, "[%s] server: detected 429/rate-limit in stderr, killing process for fallback\n", appName)
						proc.cmd.Process.Kill()
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

// attachProcessBridge sets up stdout→SSE bridge, exit watcher, and stderr drain.
// source: "server-create"/"server-resume" for full init, "" for reload.
// fullSync: true for create/resume (CC name sync + auto-rename), false for reload.
func attachProcessBridge(proc *claudeProcess, sess *serverSession, source string, fullSync bool) {
	doneCh := make(chan struct{})
	go func() {
		bridgeStdout(proc, sess.broadcaster, makeOnInit(sess, source), makeOnResult(sess, fullSync), func(todos json.RawMessage) {
			sess.setTodos(todos)
		}, makeOnMemoryAudit(sess), makeOnCanUseTool(sess))
		close(doneCh)
		// Clear bridgeDone if it still points to this generation's channel,
		// so subsequent waitBridgeDone() won't return immediately on a stale close.
		sess.mu.Lock()
		if sess.bridgeDone == doneCh {
			sess.bridgeDone = nil
		}
		sess.mu.Unlock()
	}()
	sess.mu.Lock()
	sess.bridgeDone = doneCh
	sess.mu.Unlock()
	watchExit(proc, sess)
	drainStderr(proc, sess)
}

// makeOnMemoryAudit returns a callback that logs memory operations to the audit DB.
func makeOnMemoryAudit(sess *serverSession) func(*memoryAuditEntry) {
	db := openAuditDB()
	return func(entry *memoryAuditEntry) {
		sess.mu.Lock()
		entry.SessionID = sess.ID
		entry.SessionName = sess.Name
		sess.mu.Unlock()
		go logMemoryAudit(db, *entry)
	}
}

// makeOnCanUseTool returns a callback that claims AskUserQuestion
// can_use_tool control_requests for Web UI handling. For other tools that
// unexpectedly reach this path (shouldn't happen in bypassPermissions mode
// but belt-and-suspenders), returns false so bridgeStdout auto-allows.
//
// Claims AskUserQuestion by: (1) recording a pendingAUQEntry keyed by
// request_id, (2) broadcasting an ask_user_question SSE event with the
// input so the Web UI can render the choice panel. The subprocess stays
// blocked until /api/sessions/{id}/answer-question fires and
// sendPermissionDecision unblocks it.
func makeOnCanUseTool(sess *serverSession) func(json.RawMessage) bool {
	return func(raw json.RawMessage) bool {
		var payload struct {
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype   string          `json:"subtype"`
				ToolName  string          `json:"tool_name"`
				ToolUseID string          `json:"tool_use_id"`
				Input     json.RawMessage `json:"input"`
			} `json:"request"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return false
		}
		if payload.Request.ToolName != "AskUserQuestion" {
			return false
		}

		entry := &pendingAUQEntry{
			RequestID: payload.RequestID,
			ToolUseID: payload.Request.ToolUseID,
			Input:     append(json.RawMessage(nil), payload.Request.Input...),
			CreatedAt: time.Now(),
		}
		sess.recordPendingAUQ(entry)

		// Broadcast as an SSE event so the Web UI can render the panel.
		// The frontend listens for event type "ask_user_question".
		evt, _ := json.Marshal(map[string]any{
			"request_id":  payload.RequestID,
			"tool_use_id": payload.Request.ToolUseID,
			"input":       payload.Request.Input,
		})
		sess.broadcaster.broadcast(sseEvent{Event: "ask_user_question", Data: evt})
		return true
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
	reaperDone  sync.WaitGroup // waited by shutdownAll to ensure reaper has exited
	hub         *wsHub         // WebSocket hub for real-time notifications
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
	sm.reaperDone.Add(1)
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
	ReplaceSoul bool     // 本我模式 — use --system-prompt-file instead of --append-system-prompt-file
	ResumeID    string   // Claude Code session ID to resume (adds --resume <id>)
	SpawnedBy   string   // parent session ID that spawned this one
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
	// Expand ~ in project path and validate it exists
	if opts.Project != "" {
		expanded, err := expandHome(opts.Project)
		if err != nil {
			return nil, fmt.Errorf("invalid project path %q: %w", opts.Project, err)
		}
		opts.Project = expanded
		info, err := os.Stat(opts.Project)
		if err != nil {
			return nil, fmt.Errorf("project directory %q does not exist: %w", opts.Project, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("project path %q is not a directory", opts.Project)
		}
	}

	category := opts.Category
	if category == "" {
		category = CategoryInteractive
	}

	// Fuzzy resolve model name (e.g. "glm" → "zai/glm-5.1") — defensive,
	// callers should already resolve but bare spawn via API may not.
	if opts.Model != "" {
		if resolved, err := resolveFuzzyModel(opts.Model); err == nil && resolved != "" {
			opts.Model = resolved
		} else if err != nil {
			return nil, fmt.Errorf("model %q could not be resolved: %w", opts.Model, err)
		} else {
			return nil, fmt.Errorf("model %q could not be resolved", opts.Model)
		}
	}

	// Reserve a slot atomically: check limit + register placeholder in one lock scope.
	// This prevents TOCTOU where two concurrent creates both pass the check.
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
		ReplaceSoul: opts.ReplaceSoul,
		StreamURL:   fmt.Sprintf("/api/sessions/%s/stream", id),
		mcpConfig:   opts.MCP,
		broadcaster: bc,
		hub:         sm.hub,
		GalID:       opts.GalID,
		SpawnedBy:   opts.SpawnedBy,
	}

	sm.mu.Lock()
	// Register immediately to hold the slot (status="starting")
	sm.sessions[id] = sess
	sm.mu.Unlock()

	// On failure, remove the placeholder
	cleanup := func() {
		sm.mu.Lock()
		delete(sm.sessions, id)
		sm.mu.Unlock()
	}

	// Build soul prompt if enabled
	var promptFile string
	if opts.Soul {
		initSessionDir()

		// Build prompt with per-session overrides (concurrency-safe)
		ovr := promptOverrides{
			EnvOverride: opts.EnvOverride,
		}
		if opts.Category == CategoryHeartbeat {
			ovr.Mode = "heartbeat"
		} else if opts.Category == CategoryCron {
			ovr.Mode = "cron"
		}
		// Load GAL save for resume if provided
		if opts.GalID != "" {
			galPath := filepath.Join(workspace, "memory", "gal", opts.GalID+".json")
			if data, err := os.ReadFile(galPath); err == nil {
				ovr.GalContext = string(data)
			}
		}

		result := buildPromptWithOverrides(ovr)
		promptFile = writePromptForSession(id, result)
		sess.promptFile = promptFile
	}

	// Spawn Claude Code process
	spawnOpts := sessionOpts{
		WorkDir:          opts.Project,
		SystemPromptFile: promptFile,
		Model:            opts.Model,
		MCPConfig:        opts.MCP,
		ReplaceSoul:      opts.ReplaceSoul,
		ResumeID:         opts.ResumeID,
		ServerSessionID:  id,
	}
	proc, err := spawnClaude(spawnOpts)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("spawn claude: %w", err)
	}
	sess.mu.Lock()
	sess.process = proc
	sess.mu.Unlock()
	sess.setStatus("running")

	// Register in server DB with category and tags
	ensureServerSessionFull(id, opts.Name, category, opts.Tags)
	if opts.Model != "" {
		setSessionModel(id, opts.Model)
	}
	if opts.SpawnedBy != "" {
		updateSpawnedBy(id, opts.SpawnedBy)
	}
	if opts.GalID != "" {
		setGalID(id, opts.GalID)
	}
	if opts.ReplaceSoul {
		setReplaceSoulEnabled(id, true)
	}

	// Attach stdout→SSE bridge, exit watcher, and stderr drain
	attachProcessBridge(proc, sess, "server-create", true)

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
		ServerSessionID:  sess.ID,
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

	// Wait for old bridge goroutine to exit before starting a new one,
	// preventing two bridges writing to the same broadcaster concurrently.
	sess.waitBridgeDone()

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

	// Re-attach stdout bridge (reload — no agent recording, no rename sync)
	attachProcessBridge(newProc, sess, "", false)

	if sm.hub != nil {
		sm.hub.notifySessions()
	}
	fmt.Fprintf(os.Stderr, "[%s] server: session %s chrome=%v reloaded\n", appName, shortID(id), enabled)
	return nil
}

// setReplaceSoul reloads a session's underlying claude process with
// --system-prompt-file (本我模式) toggled on/off. The serverSession
// (broadcaster, history, name, etc.) is preserved across the reload — only
// the subprocess is swapped. Note: toggling mid-session causes the system
// prompt to change, which means the model's "identity" shifts for the rest
// of the conversation. The UI warns the user about this.
func (sm *sessionManager) setReplaceSoul(id string, enabled bool) error {
	sess := sm.getSession(id)
	if sess == nil {
		return fmt.Errorf("session not found: %s", id)
	}

	sess.mu.Lock()
	if sess.ReplaceSoul == enabled && sess.process != nil && sess.process.alive() {
		sess.mu.Unlock()
		return nil // no-op
	}
	oldProc := sess.process
	resumeID := sess.ClaudeSID // preserve conversation history across reload
	opts := sessionOpts{
		WorkDir:          sess.Project,
		SystemPromptFile: sess.promptFile,
		Model:            sess.Model,
		MCPConfig:        sess.mcpConfig,
		Chrome:           sess.ChromeEnabled,
		ReplaceSoul:      enabled,
		ResumeID:         resumeID,
		ServerSessionID:  sess.ID,
	}
	sess.mu.Unlock()

	if oldProc != nil {
		oldProc.suppressClose.Store(true)
		if oldProc.alive() {
			oldProc.shutdown()
		}
	}

	// Wait for old bridge goroutine to exit before starting a new one
	sess.waitBridgeDone()

	sess.setStatus("starting")

	newProc, err := spawnClaude(opts)
	if err != nil {
		sess.setStatus("error")
		return fmt.Errorf("respawn claude: %w", err)
	}

	sess.mu.Lock()
	sess.process = newProc
	sess.ReplaceSoul = enabled
	sess.ClaudeSID = ""
	sess.mu.Unlock()

	setReplaceSoulEnabled(id, enabled)
	sess.setStatus("running")

	attachProcessBridge(newProc, sess, "", false)

	if sm.hub != nil {
		sm.hub.notifySessions()
	}
	fmt.Fprintf(os.Stderr, "[%s] server: session %s replace_soul=%v reloaded\n", appName, shortID(id), enabled)
	return nil
}

// setModel reloads the underlying claude process with a different model
// (e.g. "zai/glm-5.1"). All session state — conversation history,
// replace_soul, chrome, mcp config — is preserved across the reload.
func (sm *sessionManager) setModel(id string, model string) error {
	sess := sm.getSession(id)
	if sess == nil {
		return fmt.Errorf("session not found: %s", id)
	}

	sess.mu.Lock()
	if sess.Model == model && sess.process != nil && sess.process.alive() {
		sess.mu.Unlock()
		return nil // no-op
	}
	oldProc := sess.process
	resumeID := sess.ClaudeSID // preserve conversation history
	opts := sessionOpts{
		WorkDir:          sess.Project,
		SystemPromptFile: sess.promptFile,
		Model:            model,
		MCPConfig:        sess.mcpConfig,
		Chrome:           sess.ChromeEnabled,
		ReplaceSoul:      sess.ReplaceSoul,
		ResumeID:         resumeID,
		ServerSessionID:  sess.ID,
	}
	sess.mu.Unlock()

	if oldProc != nil {
		oldProc.suppressClose.Store(true)
		if oldProc.alive() {
			oldProc.shutdown()
		}
	}

	// Wait for old bridge goroutine to exit before starting a new one
	sess.waitBridgeDone()

	sess.setStatus("starting")

	newProc, err := spawnClaude(opts)
	if err != nil {
		sess.setStatus("error")
		return fmt.Errorf("respawn claude: %w", err)
	}

	sess.mu.Lock()
	sess.process = newProc
	sess.Model = model
	sess.ClaudeSID = ""
	sess.mu.Unlock()

	// Persist model for rehydration
	setSessionModel(sess.ID, model)

	sess.setStatus("running")

	attachProcessBridge(newProc, sess, "", false)

	if sm.hub != nil {
		sm.hub.notifySessions()
	}
	fmt.Fprintf(os.Stderr, "[%s] server: session %s model=%s reloaded\n", appName, shortID(id), model)
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

	// Mark as ended in persistent DB
	updateSessionStatus(id, "ended")

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
// Marks rehydratable sessions as "suspended" in DB before destroying processes,
// so they can be resumed on next server startup.
func (sm *sessionManager) shutdownAll() {
	close(sm.stopReaper)
	// Wait for reaper to fully exit before suspending sessions.
	// Prevents race where reaper's destroySession overwrites "suspended" → "ended".
	sm.reaperDone.Wait()

	// Persist claude_session_id and model for all active sessions before they die,
	// so they can be properly rehydrated after restart.
	sm.mu.RLock()
	for _, sess := range sm.sessions {
		sess.mu.Lock()
		sid := sess.ClaudeSID
		id := sess.ID
		model := sess.Model
		sess.mu.Unlock()
		if sid != "" {
			setClaudeSessionID(id, sid)
		}
		fmt.Fprintf(os.Stderr, "[%s] server: shutdownAll: session %s model=%q claude_sid=%s\n",
			appName, shortID(id), model, shortID(sid))
		if model != "" {
			setSessionModel(id, model)
		}
	}
	sm.mu.RUnlock()

	n := batchSuspendActiveSessions()
	if n > 0 {
		fmt.Fprintf(os.Stderr, "[%s] server: suspended %d session(s) for rehydration\n", appName, n)
	}

	sm.mu.Lock()
	ids := make([]string, 0, len(sm.sessions))
	for id := range sm.sessions {
		ids = append(ids, id)
	}
	sm.mu.Unlock()

	for _, id := range ids {
		// destroySession calls updateSessionStatus("ended"), but we want suspended
		// sessions to stay suspended. So we skip the status update for suspended sessions.
		sm.destroySessionForShutdown(id)
	}
}

// destroySessionForShutdown is like destroySession but does NOT update DB status,
// preserving the "suspended" state for rehydration.
func (sm *sessionManager) destroySessionForShutdown(id string) {
	sm.mu.Lock()
	sess, ok := sm.sessions[id]
	if !ok {
		sm.mu.Unlock()
		return
	}
	delete(sm.sessions, id)
	sm.mu.Unlock()

	sess.setStatus("stopped")
	if sess.process != nil && sess.process.alive() {
		sess.process.shutdown()
	}
	if sess.promptFile != "" {
		os.Remove(sess.promptFile)
	}
	// Do NOT call updateSessionStatus — keep "suspended" in DB
}

// reaper runs every minute and destroys idle/expired sessions.
func (sm *sessionManager) reaper() {
	defer sm.reaperDone.Done()
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
// replaceSoul: pointer — nil means "inherit from original session's persisted flag",
// non-nil means "explicitly use this value".
func (sm *sessionManager) resumeSession(sessionID, message, displayName, categoryOverride, model string, replaceSoul *bool) (*serverSession, error) {
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

	// Model priority: explicit arg > DB record > JSONL init message
	origModel := model
	if model == "" {
		model = getSessionModel(sessionID)
	}
	if model == "" {
		model = getModelFromJSONL(sessionID)
	}
	fmt.Fprintf(os.Stderr, "[%s] server: resumeSession model trace: arg=%q → resolved=%q (session %s → %s)\n",
		appName, origModel, model, shortID(sessionID), shortID(id))

	// Resolve replace_soul: explicit arg wins, else inherit from persisted DB flag
	resolvedReplaceSoul := false
	if replaceSoul != nil {
		resolvedReplaceSoul = *replaceSoul
	} else {
		resolvedReplaceSoul = getReplaceSoulEnabled(sessionID)
	}

	// Inherit first_msg from original session
	inheritedFirstMsg := getFirstMsgFromJSONL(sessionID)

	sess := &serverSession{
		ID:          id,
		Name:        displayName,
		FirstMsg:    inheritedFirstMsg,
		ResumedFrom: sessionID,
		Project:     workspace,
		Model:       model,
		Status:      "starting",
		Category:    resolvedCategory,
		CreatedAt:   now,
		LastActive:  now,
		SoulEnabled: true,
		ReplaceSoul: resolvedReplaceSoul,
		StreamURL:   fmt.Sprintf("/api/sessions/%s/stream", id),
		broadcaster: bc,
		hub:         sm.hub,
	}

	// Reserve slot atomically: dedup check + maxSessions check + register in one lock scope.
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
	// Register placeholder to hold the slot
	sm.sessions[id] = sess
	sm.mu.Unlock()

	cleanup := func() {
		sm.mu.Lock()
		delete(sm.sessions, id)
		sm.mu.Unlock()
	}

	// Build soul prompt — per-session file to avoid concurrent overwrites
	promptFile := writePromptForSession(id, buildPromptWithOverrides(promptOverrides{}))
	sess.promptFile = promptFile

	// Spawn Claude Code with --resume
	proc, err := spawnClaude(sessionOpts{
		SystemPromptFile: promptFile,
		Model:            model,
		ResumeID:         sessionID,
		ReplaceSoul:      resolvedReplaceSoul,
		ServerSessionID:  id,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("spawn resume: %w", err)
	}

	sess.mu.Lock()
	sess.process = proc
	sess.mu.Unlock()
	sess.setStatus("running")

	// Register in server DB
	ensureServerSession(id, sess.Name)
	if model != "" {
		setSessionModel(id, model)
	}
	if resolvedReplaceSoul {
		setReplaceSoulEnabled(id, true)
	}

	// Attach stdout bridge with full sync (agent recording + CC name sync + auto-rename)
	attachProcessBridge(proc, sess, "server-resume", true)

	// Send initial message if provided — wait for Claude Code to emit init
	// before writing to stdin (consistent with createSession's waitInit pattern).
	if message != "" {
		sess.mu.Lock()
		if sess.FirstMsg == "" {
			sess.FirstMsg = message
		}
		sess.mu.Unlock()
		if !proc.waitInit(30 * time.Second) {
			fmt.Fprintf(os.Stderr, "[%s] server: init timeout for resume %s, sending message anyway\n", appName, shortID(sess.ID))
		}
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
	sessDir := filepath.Join(claudeConfigDir, "sessions")
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

// ── Session Rehydration ──

const rehydrateMaxAge = 2 * time.Hour

// rehydrateSessions restores interactive/telegram sessions that were alive
// before the last server restart. Called once during server startup.
func (sm *sessionManager) rehydrateSessions() {
	sessions, err := getRehydratableSessions(rehydrateMaxAge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: query error: %v\n", appName, err)
		return
	}
	if len(sessions) == 0 {
		fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: no sessions to restore\n", appName)
		// Clean up any stale active/suspended records
		expireStaleRehydratables(rehydrateMaxAge)
		return
	}

	fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: found %d session(s) to restore\n", appName, len(sessions))

	restored := 0
	for _, s := range sessions {
		// Verify the JSONL file exists (claude session is resumable)
		jsonlPath := findSessionJSONL(s.ClaudeSID)
		if jsonlPath == "" {
			fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: skip %s (no JSONL for %s)\n",
				appName, shortID(s.SessionID), shortID(s.ClaudeSID))
			updateSessionStatus(s.SessionID, "ended")
			continue
		}

		// Determine the resume message:
		// - Restart initiator: use their custom rehydrate_message
		// - Bystander sessions: inject a server-restart context notice so the
		//   model knows it was interrupted and won't hallucinate completed tool calls
		resumeMsg := s.RehydrateMsg
		if resumeMsg == "" {
			resumeMsg = "⚠️ Server was restarted. Your previous session was interrupted — " +
				"any in-flight tool calls (Bash, etc.) were killed and did NOT complete. " +
				"Do NOT assume they succeeded. Report your current status and wait for instructions. " +
				"IMPORTANT: If YOU triggered this restart (e.g. via `make restart`, `launchctl stop/start`, or similar), " +
				"do NOT restart again — it already happened."
		}

		fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: restoring %s (model=%q, claude_sid=%s)\n",
			appName, shortID(s.SessionID), s.Model, shortID(s.ClaudeSID))
		replaceSoul := s.ReplaceSoul
		sess, err := sm.resumeSession(
			s.ClaudeSID,        // sessionID to resume
			resumeMsg,          // always send a message (context notice or wake command)
			s.Name,             // display name
			s.Category,         // category override
			s.Model,            // model
			&replaceSoul,       // replace soul flag
		)
		// Always mark old record as ended (whether resume succeeded or not)
		updateSessionStatus(s.SessionID, "ended")
		if s.RehydrateMsg != "" {
			setRehydrateMessage(s.SessionID, "")
		}

		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: failed %s: %v\n",
				appName, shortID(s.SessionID), err)
			continue
		}

		// Carry over chrome/gal state to new session
		if s.ChromeEnabled {
			setChromeEnabled(sess.ID, true)
		}
		if s.GalID != "" {
			setGalID(sess.ID, s.GalID)
		}

		wakeNote := "idle"
		if s.RehydrateMsg != "" {
			wakeNote = "wake"
		}
		fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: restored %s → %s (%s, %s)\n",
			appName, shortID(s.SessionID), shortID(sess.ID), s.Category, wakeNote)
		restored++
	}

	// Expire any remaining stale sessions
	expireStaleRehydratables(rehydrateMaxAge)

	fmt.Fprintf(os.Stderr, "[%s] server: rehydrate: restored %d/%d session(s)\n",
		appName, restored, len(sessions))

	// Notify WS clients about restored sessions
	if sm.hub != nil {
		sm.hub.notifySessions()
	}
}

// ── JSONL History Parsing ──

// findSessionJSONL locates the JSONL file for a given session ID.
// Searches both Claude Code projects and OpenClaw agent session directories.
func findSessionJSONL(sessionID string) string {
	fname := sessionID + ".jsonl"

	// Claude Code sessions: ~/.claude/projects/*/
	claudeProjects := filepath.Join(claudeConfigDir, "projects")
	if entries, err := os.ReadDir(claudeProjects); err == nil {
		for _, projEntry := range entries {
			if !projEntry.IsDir() {
				continue
			}
			path := filepath.Join(claudeProjects, projEntry.Name(), fname)
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
	}

	// OpenClaw sessions: ~/.openclaw/agents/*/sessions/
	home, _ := os.UserHomeDir()
	ocAgents := filepath.Join(home, ".openclaw", "agents")
	if entries, err := os.ReadDir(ocAgents); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(ocAgents, e.Name(), "sessions", fname)
			if _, err := os.Stat(path); err == nil {
				return path
			}
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

		case "system":
			var sysPeek struct {
				Subtype     string `json:"subtype"`
				CompactMeta struct {
					Trigger   string `json:"trigger"`
					PreTokens int    `json:"preTokens"`
					PreTokens2 int   `json:"pre_tokens"`
				} `json:"compactMetadata"`
				CompactMeta2 struct {
					Trigger   string `json:"trigger"`
					PreTokens int    `json:"preTokens"`
					PreTokens2 int   `json:"pre_tokens"`
				} `json:"compact_metadata"`
			}
			if json.Unmarshal(line, &sysPeek) == nil && sysPeek.Subtype == "compact_boundary" {
				// Merge both naming conventions: pick non-zero from either struct
				trigger := sysPeek.CompactMeta.Trigger
				if trigger == "" {
					trigger = sysPeek.CompactMeta2.Trigger
				}
				tokens := sysPeek.CompactMeta.PreTokens
				if tokens == 0 {
					tokens = sysPeek.CompactMeta.PreTokens2
				}
				if tokens == 0 {
					tokens = sysPeek.CompactMeta2.PreTokens
				}
				if tokens == 0 {
					tokens = sysPeek.CompactMeta2.PreTokens2
				}
				all = append(all, historyMessage{
					Role:      "compact_boundary",
					Content:   fmt.Sprintf("compact_boundary|%s|%d", trigger, tokens),
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
					// Keep full input for code-edit tools (Edit/Write/NotebookEdit/MultiEdit)
					// so frontend can render diffs; truncate others
					switch b.Name {
					case "Edit", "Write", "NotebookEdit", "MultiEdit":
						// no truncation
					default:
						if len(inputStr) > 500 {
							inputStr = inputStr[:500] + "..."
						}
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
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] history scanner error for %s: %v\n", appName, filepath.Base(path), err)
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
