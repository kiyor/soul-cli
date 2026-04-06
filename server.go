package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

//go:embed web/index.html
var indexHTML []byte

// ── Server Config ──

type serverConfig struct {
	Token           string `json:"token"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	MaxSessions     int    `json:"maxSessions"`
	IdleTimeoutMin  int    `json:"idleTimeoutMin"`
	MaxLifetimeHrs  int    `json:"maxLifetimeHours"`
	RateLimitPerMin int    `json:"rateLimitPerMin"`
}

func defaultServerConfig() serverConfig {
	return serverConfig{
		Host:            "0.0.0.0",
		Port:            9847,
		MaxSessions:     5,
		IdleTimeoutMin:  30,
		MaxLifetimeHrs:  4,
		RateLimitPerMin: 60,
	}
}

// loadServerConfig reads server config from config.json's "server" field.
func loadServerConfig() serverConfig {
	cfg := defaultServerConfig()

	configPaths := []string{
		appDir + "/config.json",
	}
	for _, p := range configPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var wrapper struct {
			Server serverConfig `json:"server"`
		}
		if json.Unmarshal(data, &wrapper) == nil && (wrapper.Server.Token != "" || wrapper.Server.Port != 0) {
			if wrapper.Server.Token != "" {
				cfg.Token = wrapper.Server.Token
			}
			if wrapper.Server.Host != "" {
				cfg.Host = wrapper.Server.Host
			}
			if wrapper.Server.Port != 0 {
				cfg.Port = wrapper.Server.Port
			}
			if wrapper.Server.MaxSessions > 0 {
				cfg.MaxSessions = wrapper.Server.MaxSessions
			}
			if wrapper.Server.IdleTimeoutMin > 0 {
				cfg.IdleTimeoutMin = wrapper.Server.IdleTimeoutMin
			}
			if wrapper.Server.MaxLifetimeHrs > 0 {
				cfg.MaxLifetimeHrs = wrapper.Server.MaxLifetimeHrs
			}
			if wrapper.Server.RateLimitPerMin > 0 {
				cfg.RateLimitPerMin = wrapper.Server.RateLimitPerMin
			}
		}
		break
	}

	return cfg
}

// ── Rate Limiter (per-token, sliding window) ──

type rateLimiter struct {
	mu       sync.Mutex
	requests []time.Time
	limit    int
	window   time.Duration
}

func newRateLimiter(limit int) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: time.Minute,
	}
}

func (rl *rateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Trim old entries
	valid := 0
	for _, t := range rl.requests {
		if t.After(cutoff) {
			rl.requests[valid] = t
			valid++
		}
	}
	rl.requests = rl.requests[:valid]

	if len(rl.requests) >= rl.limit {
		return false
	}
	rl.requests = append(rl.requests, now)
	return true
}

// ── HTTP Server ──

// handleServer is the main entry point for `weiran server`.
func handleServer(args []string) {
	cfg := loadServerConfig()

	// Override from CLI flags
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &cfg.Port)
				i++
			}
		case "--host":
			if i+1 < len(args) {
				cfg.Host = args[i+1]
				i++
			}
		case "--token":
			if i+1 < len(args) {
				cfg.Token = args[i+1]
				i++
			}
		}
	}

	// Token from env overrides
	if envToken := os.Getenv("WEIRAN_SERVER_TOKEN"); envToken != "" {
		cfg.Token = envToken
	}

	// Refuse to start without auth token
	if cfg.Token == "" {
		fmt.Fprintf(os.Stderr, "[%s] server: refusing to start without auth token\n", appName)
		fmt.Fprintf(os.Stderr, "  set via: --token, WEIRAN_SERVER_TOKEN env, or config.json server.token\n")
		os.Exit(1)
	}

	// Info if binding to non-localhost
	if cfg.Host != "127.0.0.1" && cfg.Host != "localhost" && cfg.Host != "::1" {
		fmt.Fprintf(os.Stderr, "[%s] server: binding to %s (non-localhost)\n", appName, cfg.Host)
	}

	sm := newSessionManager(
		cfg.MaxSessions,
		time.Duration(cfg.IdleTimeoutMin)*time.Minute,
		time.Duration(cfg.MaxLifetimeHrs)*time.Hour,
	)

	rl := newRateLimiter(cfg.RateLimitPerMin)

	mux := http.NewServeMux()

	// Health (no auth required)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "ok",
			"app":        appName,
			"agent_name": agentName,
			"version":    buildVersion,
			"sessions":   len(sm.sessions),
			"uptime":     time.Since(serverStartTime).String(),
		})
	})

	// Config info (authed)
	mux.HandleFunc("GET /api/config", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"max_sessions":     cfg.MaxSessions,
			"idle_timeout_min": cfg.IdleTimeoutMin,
			"max_lifetime_hrs": cfg.MaxLifetimeHrs,
			"rate_limit":       cfg.RateLimitPerMin,
			"workspace":        workspace,
			"claude_bin":       claudeBin,
		})
	}))

	// List sessions
	mux.HandleFunc("GET /api/sessions", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sm.listSessions())
	}))

	// Create session
	mux.HandleFunc("POST /api/sessions", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		var req struct {
			Name           string `json:"name"`
			Project        string `json:"project"`
			Model          string `json:"model"`
			InitialMessage string `json:"initial_message"`
			SoulFiles      bool   `json:"soul_files"`
			MCPConfig      string `json:"mcp_config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}

		if req.Name == "" {
			req.Name = fmt.Sprintf("session-%s", time.Now().Format("0102-1504"))
		}
		if req.Project == "" {
			req.Project = workspace
		}

		sess, err := sm.createSession(req.Name, req.Project, req.Model, req.SoulFiles, req.MCPConfig)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}

		fmt.Fprintf(os.Stderr, "[%s] server: session created: %s (%s)\n", appName, shortID(sess.ID), sess.Name)

		// Send initial message if provided
		if req.InitialMessage != "" {
			// Wait briefly for Claude Code to initialize
			time.Sleep(500 * time.Millisecond)
			if err := sess.process.sendMessage(req.InitialMessage); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] server: failed to send initial message: %v\n", appName, err)
			}
		}

		writeJSON(w, http.StatusCreated, sess.snapshot())
	}))

	// Get session
	mux.HandleFunc("GET /api/sessions/{id}", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusOK, sess.snapshot())
	}))

	// Delete session
	mux.HandleFunc("DELETE /api/sessions/{id}", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := sm.destroySession(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		fmt.Fprintf(os.Stderr, "[%s] server: session destroyed: %s\n", appName, shortID(id))
		writeJSON(w, http.StatusOK, map[string]string{"status": "destroyed"})
	}))

	// Send message to session
	mux.HandleFunc("POST /api/sessions/{id}/message", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		var req struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
			return
		}

		if !sess.process.alive() {
			writeJSON(w, http.StatusGone, map[string]string{"error": "session process has exited"})
			return
		}

		sess.touch()
		sess.setStatus("running")
		if err := sess.process.sendMessage(req.Message); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	}))

	// SSE stream
	mux.HandleFunc("GET /api/sessions/{id}/stream", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		serveSSE(w, r, sess.broadcaster)
	}))

	// Control request (interrupt, set_model, etc.)
	mux.HandleFunc("POST /api/sessions/{id}/control", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		var req struct {
			Subtype string         `json:"subtype"`
			Extra   map[string]any `json:"extra"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Subtype == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subtype is required"})
			return
		}

		if err := sess.process.controlRequest(req.Subtype, req.Extra); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	}))

	// Historical sessions (from JSONL files, for resume)
	mux.HandleFunc("GET /api/history", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		limit := 30
		if l := r.URL.Query().Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 100 {
				limit = v
			}
		}

		all := scanAllSessions()
		sort.Slice(all, func(i, j int) bool { return all[i].ModTime.After(all[j].ModTime) })
		if len(all) > limit {
			all = all[:limit]
		}

		// Convert to JSON-friendly format
		items := make([]map[string]any, 0, len(all))
		for _, s := range all {
			items = append(items, map[string]any{
				"id":        s.ID,
				"name":      s.Name,
				"title":     s.Title,
				"project":   s.Project,
				"model":     s.Model,
				"first_msg": s.FirstMsg,
				"summary":   s.Summary,
				"size":      s.Size,
				"messages":  s.Messages,
				"mod_time":  s.ModTime.Format(time.RFC3339),
			})
		}
		writeJSON(w, http.StatusOK, items)
	}))

	// Get messages from a historical session JSONL file
	mux.HandleFunc("GET /api/history/{id}/messages", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")

		// Find the JSONL file
		path := findSessionJSONL(sessionID)
		if path == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session file not found"})
			return
		}

		limit := 200
		if l := r.URL.Query().Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 1000 {
				limit = v
			}
		}

		messages := parseSessionMessages(path, limit)
		writeJSON(w, http.StatusOK, messages)
	}))

	// Resume a historical session (creates a server session wrapping --resume)
	mux.HandleFunc("POST /api/sessions/resume", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		var req struct {
			SessionID string `json:"session_id"`
			Message   string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session_id is required"})
			return
		}

		sm.mu.Lock()
		if len(sm.sessions) >= sm.maxSessions {
			sm.mu.Unlock()
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": fmt.Sprintf("max sessions reached (%d)", sm.maxSessions)})
			return
		}
		sm.mu.Unlock()

		id := uuid.New().String()
		now := time.Now()

		sess := &serverSession{
			ID:          id,
			Name:        "resume-" + shortID(req.SessionID),
			Project:     workspace,
			Status:      "starting",
			CreatedAt:   now,
			LastActive:  now,
			SoulEnabled: true,
			StreamURL:   fmt.Sprintf("/api/sessions/%s/stream", id),
			broadcaster: newBroadcaster(),
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
			"--resume", req.SessionID,
		}

		cmd := exec.Command(claudeBin, args...)
		cmd.Dir = workspace
		env := filterEnv(os.Environ(), "CLAUDECODE")
		env = append(env, "CLAUDE_CODE_ENTRYPOINT=sdk-go")
		cmd.Env = env

		stdin, err := cmd.StdinPipe()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		if err := cmd.Start(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "spawn: " + err.Error()})
			return
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

		go bridgeStdout(proc, sess.broadcaster, func(raw json.RawMessage) {
			var res ResultMessage
			if json.Unmarshal(raw, &res) == nil {
				sess.mu.Lock()
				sess.TotalCost += res.TotalCostUSD
				sess.NumTurns += res.NumTurns
				if res.Subtype == "error" {
					sess.Status = "error"
				} else {
					sess.Status = "idle"
				}
				if res.SessionID != "" {
					sess.ClaudeSID = res.SessionID
				}
				sess.mu.Unlock()
			}
			sess.touch()
		})

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
		if req.Message != "" {
			time.Sleep(500 * time.Millisecond)
			proc.sendMessage(req.Message)
		}

		fmt.Fprintf(os.Stderr, "[%s] server: resumed session %s as %s\n", appName, shortID(req.SessionID), shortID(id))
		writeJSON(w, http.StatusCreated, sess.snapshot())
	}))

	// Serve UI
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	// Start server
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE needs no write timeout
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\n[%s] server: received %v, shutting down...\n", appName, sig)

		// Notify via Telegram
		if tgChatID != "" {
			trySendTelegram(fmt.Sprintf("🔴 %s server shutting down (signal: %v)", appName, sig))
		}

		sm.shutdownAll()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	serverStartTime = time.Now()
	fmt.Fprintf(os.Stderr, "[%s] server: listening on %s\n", appName, addr)
	fmt.Fprintf(os.Stderr, "[%s] server: max_sessions=%d idle_timeout=%dm max_lifetime=%dh\n",
		appName, cfg.MaxSessions, cfg.IdleTimeoutMin, cfg.MaxLifetimeHrs)

	// Notify via Telegram
	if tgChatID != "" {
		trySendTelegram(fmt.Sprintf("🟢 %s server started on %s", appName, addr))
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "[%s] server: fatal: %v\n", appName, err)
		os.Exit(1)
	}
}

var serverStartTime time.Time

// ── Middleware ──

// authMiddleware verifies Bearer token.
func authMiddleware(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			// Also check query parameter (for SSE clients that can't set headers)
			if q := r.URL.Query().Get("token"); q == token {
				next(w, r)
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing Authorization header"})
			return
		}

		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || parts[1] != token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}

		next(w, r)
	}
}

// ── Helpers ──

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
