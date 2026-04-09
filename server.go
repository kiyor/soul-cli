package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

// ── Server Config ──

type serverConfig struct {
	Token           string              `json:"token"`
	Host            string              `json:"host"`
	Port            int                 `json:"port"`
	MaxSessions     int                 `json:"maxSessions"`
	IdleTimeoutMin  int                 `json:"idleTimeoutMin"`
	MaxLifetimeHrs  int                 `json:"maxLifetimeHours"`
	RateLimitPerMin int                 `json:"rateLimitPerMin"`
	Telegram        telegramBotConfig   `json:"telegram"`
	Proxy           proxyConfig         `json:"proxy"`
	SessionReset    sessionResetConfig  `json:"sessionReset"`
}

// sessionResetConfig mirrors SessionResetPolicy for JSON config loading.
// Fields default to zero-values; loadResetPolicyFromConfig fills in sane
// defaults for anything unset.
type sessionResetConfig struct {
	Mode          string `json:"mode"`          // "idle" | "daily" | "both" | "none"
	IdleMinutes   int    `json:"idleMinutes"`   // 0 → default 1440
	DailyAtHour   int    `json:"dailyAtHour"`   // 0-23; default 4
	NotifyOnReset bool   `json:"notifyOnReset"` // send Telegram on reset
}

// telegramBotConfig holds Telegram bot settings.
type telegramBotConfig struct {
	Enabled        bool     `json:"enabled"`
	AllowedChatIDs []string `json:"allowedChatIDs"` // if empty, uses global tgChatID
}

func defaultServerConfig() serverConfig {
	return serverConfig{
		Host:            "0.0.0.0",
		Port:            9847,
		MaxSessions:     5,
		IdleTimeoutMin:  30,
		MaxLifetimeHrs:  4,
		RateLimitPerMin: 60,
		Proxy:           defaultProxyConfig(),
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
		if json.Unmarshal(data, &wrapper) == nil && (wrapper.Server.Token != "" || wrapper.Server.Port != 0 || wrapper.Server.Telegram.Enabled) {
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
			cfg.Telegram = wrapper.Server.Telegram
			// Session reset policy (always copy; defaults are filled in by loadResetPolicyFromConfig)
			cfg.SessionReset = wrapper.Server.SessionReset
			if wrapper.Server.Proxy.Port != 0 || wrapper.Server.Proxy.Upstream != "" || wrapper.Server.Proxy.Enabled {
				if wrapper.Server.Proxy.Enabled {
					cfg.Proxy.Enabled = true
				}
				if wrapper.Server.Proxy.Port != 0 {
					cfg.Proxy.Port = wrapper.Server.Proxy.Port
				}
				if wrapper.Server.Proxy.Upstream != "" {
					cfg.Proxy.Upstream = wrapper.Server.Proxy.Upstream
				}
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
	isServerMode = true
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

	// WebSocket hub for bidirectional real-time sync
	hub := newWSHub(cfg.Token, rl)
	hub.sm = sm
	sm.hub = hub

	mux := http.NewServeMux()

	// Health (no auth required)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "ok",
			"app":           appName,
			"agent_name":    agentName,
			"version":       buildVersion,
			"sessions":      len(sm.sessions),
			"uptime":        time.Since(serverStartTime).String(),
			"sniff_enabled": activeProxyPort > 0,
			"sniff_port":    activeProxyPort,
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

	// FTS5 full-text search over daily notes + session summaries
	// GET /api/search?q=<query>&scope=<daily|session|both>&limit=<N>
	mux.HandleFunc("GET /api/search", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q required"})
			return
		}
		scope := r.URL.Query().Get("scope")
		if scope == "" {
			scope = "both"
		}
		limit := 20
		if l := r.URL.Query().Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
			if limit < 1 || limit > 200 {
				limit = 20
			}
		}
		hits, err := searchFTS(q, scope, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"query": q,
			"scope": scope,
			"hits":  hits,
			"total": len(hits),
		})
	}))

	// Link preview (OG tags)
	mux.HandleFunc("GET /api/link-preview", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		url := r.URL.Query().Get("url")
		if url == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url required"})
			return
		}
		data := fetchOGTags(url)
		if data == nil {
			writeJSON(w, http.StatusNoContent, nil)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		writeJSON(w, http.StatusOK, data)
	}))

	// List configured providers (for UI model dropdown, apiKey redacted)
	mux.HandleFunc("GET /api/providers", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		type providerInfo struct {
			Name   string   `json:"name"`
			Models []string `json:"models,omitempty"`
		}
		// Read providers from config.json
		configPaths := []string{
			filepath.Join(appHome, "data", "config.json"),
			filepath.Join(workspace, "scripts", appName, "config.json"),
		}
		var providers []providerInfo
		for _, p := range configPaths {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var raw struct {
				Providers map[string]struct {
					BaseURL string   `json:"baseUrl"`
					Models  []string `json:"models,omitempty"`
				} `json:"providers"`
			}
			if json.Unmarshal(data, &raw) == nil && len(raw.Providers) > 0 {
				for name, prov := range raw.Providers {
					if prov.BaseURL == "" {
						continue
					}
					providers = append(providers, providerInfo{Name: name, Models: prov.Models})
				}
				break
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"providers":    providers,
			"defaultModel": defaultModel,
		})
	}))

	// List available skills
	mux.HandleFunc("GET /api/skills", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		var skills []map[string]string
		for _, dir := range skillDirs {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				skillMd := dir + "/" + e.Name() + "/SKILL.md"
				if _, err := os.Stat(skillMd); err != nil {
					continue
				}
				name, desc := parseSkillFrontmatter(skillMd)
				if name == "" {
					name = e.Name()
				}
				// truncate description
				for _, cut := range []string{"Trigger", "trigger", "触发"} {
					if idx := strings.Index(desc, cut); idx > 0 {
						desc = strings.TrimSpace(desc[:idx])
						desc = strings.TrimRight(desc, "。.\n :：,，")
						break
					}
				}
				if len(desc) > 80 {
					desc = desc[:80] + "…"
				}
				skills = append(skills, map[string]string{"name": name, "description": desc})
			}
		}
		writeJSON(w, http.StatusOK, skills)
	}))

	// List sessions (optional ?category=interactive to filter)
	mux.HandleFunc("GET /api/sessions", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		all := sm.listSessions()
		cat := r.URL.Query().Get("category")
		if cat == "" {
			writeJSON(w, http.StatusOK, all)
			return
		}
		filtered := make([]map[string]any, 0)
		for _, s := range all {
			if s["category"] == cat {
				filtered = append(filtered, s)
			}
		}
		writeJSON(w, http.StatusOK, filtered)
	}))

	// Create session
	mux.HandleFunc("POST /api/sessions", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		var req struct {
			Name           string   `json:"name"`
			Project        string   `json:"project"`
			Model          string   `json:"model"`
			InitialMessage string   `json:"initial_message"`
			SoulFiles      bool     `json:"soul_files"`
			MCPConfig      string   `json:"mcp_config"`
			GalID          string   `json:"gal_id"`
			Category       string   `json:"category"`
			Tags           []string `json:"tags"`
			ReplaceSoul    bool     `json:"replace_soul"`
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

		sess, err := sm.createSessionWithOpts(sessionCreateOpts{
			Name:        req.Name,
			Project:     req.Project,
			Model:       req.Model,
			Soul:        req.SoulFiles,
			MCP:         req.MCPConfig,
			GalID:       req.GalID,
			Category:    req.Category,
			Tags:        req.Tags,
			ReplaceSoul: req.ReplaceSoul,
		})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}

		fmt.Fprintf(os.Stderr, "[%s] server: session created: %s (%s)\n", appName, shortID(sess.ID), sess.Name)

		// Send initial message if provided
		if req.InitialMessage != "" {
			// Wait briefly for Claude Code to initialize
			time.Sleep(500 * time.Millisecond)
			userEvent, _ := json.Marshal(map[string]any{
				"type":    "user",
				"message": map[string]any{"role": "user", "content": req.InitialMessage},
			})
			sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})
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

	// Rename session
	mux.HandleFunc("PATCH /api/sessions/{id}/rename", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		sess.mu.Lock()
		sess.Name = req.Name
		sess.mu.Unlock()
		markRenamed(sess.ID, req.Name)
		if hub != nil {
			hub.notifySessions()
		}
		fmt.Fprintf(os.Stderr, "[%s] server: session %s renamed to %q\n", appName, shortID(sess.ID), req.Name)
		writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "name": req.Name})
	}))

	// Auto-rename session (calls Haiku to generate title from conversation)
	mux.HandleFunc("POST /api/sessions/{id}/auto-rename", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		// Collect snippets from broadcaster history (same logic as tryAutoRename)
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
			writeJSON(w, http.StatusOK, map[string]string{"error": "no conversation context for auto-rename"})
			return
		}
		context := strings.Join(snippets, "\n---\n")
		if len(context) > 1500 {
			context = context[:1500]
		}
		title := callHaikuForTitle(context)
		if title == "" {
			writeJSON(w, http.StatusOK, map[string]string{"error": "no title generated"})
			return
		}
		sess.mu.Lock()
		sess.Name = title
		sess.mu.Unlock()
		markAutoNamed(sess.ID, title)
		if hub != nil {
			hub.notifySessions()
		}
		fmt.Fprintf(os.Stderr, "[%s] server: auto-renamed %s → %q\n", appName, shortID(sess.ID), title)
		writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "name": title})
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

		// Broadcast user message to SSE/WS so it persists in history
		// (without this, switching sessions and back loses user messages)
		userEvent, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": req.Message},
		})
		sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})

		if err := sess.process.sendMessage(req.Message); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	}))

	// Upload file to session (multipart)
	mux.HandleFunc("POST /api/sessions/{id}/upload", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		// Parse multipart form — 32 MB max
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse multipart form: " + err.Error()})
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file field is required"})
			return
		}
		defer file.Close()

		// Validate file type (images only for now)
		ext := strings.ToLower(filepath.Ext(header.Filename))
		allowedExts := map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true}
		if !allowedExts[ext] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported file type: " + ext})
			return
		}

		// Ensure content-type matches
		ct := header.Header.Get("Content-Type")
		if ct == "" {
			ct = mime.TypeByExtension(ext)
		}

		// Generate unique filename: timestamp-random.ext
		randBytes := make([]byte, 8)
		rand.Read(randBytes)
		filename := fmt.Sprintf("%d-%s%s", time.Now().UnixMilli(), hex.EncodeToString(randBytes), ext)

		// Save to workspace/uploads/
		uploadsDir := filepath.Join(workspace, "uploads")
		if err := os.MkdirAll(uploadsDir, 0755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create uploads dir: " + err.Error()})
			return
		}

		destPath := filepath.Join(uploadsDir, filename)
		dst, err := os.Create(destPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create file: " + err.Error()})
			return
		}
		defer dst.Close()

		written, err := io.Copy(dst, file)
		if err != nil {
			os.Remove(destPath)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save file: " + err.Error()})
			return
		}

		// Build URL path for the uploaded file
		urlPath := "/uploads/" + filename

		fmt.Fprintf(os.Stderr, "[%s] server: uploaded %s (%d bytes) for session %s\n",
			appName, filename, written, sess.ID)

		writeJSON(w, http.StatusOK, map[string]any{
			"url":           urlPath,
			"filename":      filename,
			"original_name": header.Filename,
			"size":          written,
			"content_type":  ct,
		})
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

	// Toggle chrome (--chrome flag) — reloads the underlying claude proc.
	mux.HandleFunc("POST /api/sessions/{id}/chrome", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if err := sm.setChrome(sess.ID, req.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "chrome_enabled": req.Enabled})
	}))

	// Toggle 本我模式 (replace-soul) — reloads the underlying claude proc
	// with --system-prompt-file instead of --append-system-prompt-file.
	// Strips CC's native system prompt, leaving only the soul. Server-side
	// Anthropic safety blocks (injection defense, privacy, copyright) are
	// NOT affected — they're baked into the API. Mid-session toggling
	// causes the model's "identity" to shift for the remainder of the
	// conversation; the UI warns about this.
	mux.HandleFunc("POST /api/sessions/{id}/replace-soul", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if err := sm.setReplaceSoul(sess.ID, req.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "replace_soul": req.Enabled})
	}))

	// Switch model — respawns the claude process with a new model while
	// preserving session state (conversation history, replace_soul, chrome).
	mux.HandleFunc("POST /api/sessions/{id}/set-model", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
			return
		}
		if err := sm.setModel(sess.ID, req.Model); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "model": req.Model})
	}))

	// Context usage — fire control request, response comes via SSE
	mux.HandleFunc("POST /api/sessions/{id}/usage", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		if err := sess.process.controlRequest("get_context_usage", nil); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	}))

	// Historical sessions (from JSONL files, for resume)
	// ?category=interactive — filter by category (looks up server_sessions DB)
	// ?category=all — show all including heartbeat/cron (default: interactive only)
	mux.HandleFunc("GET /api/history", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		limit := 30
		if l := r.URL.Query().Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 100 {
				limit = v
			}
		}
		catFilter := r.URL.Query().Get("category")
		if catFilter == "" {
			catFilter = CategoryInteractive // default: hide ephemeral
		}

		all := scanAllSessions()
		sort.Slice(all, func(i, j int) bool { return all[i].ModTime.After(all[j].ModTime) })

		// Build spawn agent lookup: session_name → agent_id
		spawnAgentMap := loadSpawnAgentMap()

		// Convert to JSON-friendly format, applying category filter
		items := make([]map[string]any, 0, len(all))
		for _, s := range all {
			// Look up category from DB via Claude session ID
			cat := getSessionCategoryByClaudeSID(s.ID)
			if cat == "" {
				cat = CategoryInteractive // sessions without DB records are legacy interactive
			}

			// Filter by category (unless "all" is requested)
			if catFilter != "all" && cat != catFilter {
				continue
			}

			if len(items) >= limit {
				break
			}

			// Determine agent: DB record > spawn name match > default "main" for server sessions
			agent := getSessionAgent(s.ID)
			if agent == "" {
				// Check if session name matches a spawn pattern
				if a, ok := spawnAgentMap[s.Name]; ok {
					agent = a
				}
			}

			cost := getSessionCostByClaudeSID(s.ID)
			items = append(items, map[string]any{
				"id":            s.ID,
				"name":          s.Name,
				"title":         s.Title,
				"project":       s.Project,
				"model":         s.Model,
				"category":      cat,
				"agent":         agent,
				"first_msg":     s.FirstMsg,
				"summary":       s.Summary,
				"size":          s.Size,
				"messages":      s.Messages,
				"mod_time":      s.ModTime.Format(time.RFC3339),
				"proxy_cost_usd": cost,
			})
		}
		writeJSON(w, http.StatusOK, items)
	}))

	// Get messages from a historical session JSONL file
	mux.HandleFunc("GET /api/history/{id}/messages", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")

		// Find the JSONL file — try direct ID first, then check active sessions
		// for ClaudeSID or ResumedFrom mapping
		path := findSessionJSONL(sessionID)
		if path == "" {
			if sess := sm.getSession(sessionID); sess != nil {
				if sess.ClaudeSID != "" {
					path = findSessionJSONL(sess.ClaudeSID)
				}
				if path == "" && sess.ResumedFrom != "" {
					path = findSessionJSONL(sess.ResumedFrom)
				}
			}
		}
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
			SessionID   string `json:"session_id"`
			Message     string `json:"message"`
			Name        string `json:"name"`
			Category    string `json:"category"`
			Model       string `json:"model"`
			ReplaceSoul *bool  `json:"replace_soul"` // nil → inherit persisted DB flag
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session_id is required"})
			return
		}

		sess, err := sm.resumeSession(req.SessionID, req.Message, req.Name, req.Category, req.Model, req.ReplaceSoul)
		if err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "max sessions") {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusCreated, sess.snapshot())
	}))

	// GAL save endpoints
	mux.HandleFunc("GET /api/gal", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		galDir := filepath.Join(workspace, "memory", "gal")
		entries, err := os.ReadDir(galDir)
		if err != nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		var saves []map[string]any
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(galDir, e.Name()))
			if err != nil {
				continue
			}
			var save map[string]any
			if json.Unmarshal(data, &save) == nil {
				saves = append(saves, save)
			}
		}
		if saves == nil {
			saves = []map[string]any{}
		}
		writeJSON(w, http.StatusOK, saves)
	}))

	mux.HandleFunc("GET /api/gal/{id}", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		galPath := filepath.Join(workspace, "memory", "gal", id+".json")
		data, err := os.ReadFile(galPath)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "save not found"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))

	// Wake API — trigger a heartbeat session (replaces OpenClaw /hooks/wake)
	// Body: {"text": "reason", "soul": true/false}
	// soul=true (default) → full soul prompt; soul=false → bare claude, lighter context
	// No auth required — Jira calls this from Docker container
	mux.HandleFunc("POST /api/wake", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text  string `json:"text"`
			Soul  *bool  `json:"soul"`  // pointer to distinguish missing from false
			Model string `json:"model"` // override model (e.g. from defaultModel via delegateToServer)
		}
		json.NewDecoder(r.Body).Decode(&req) // optional body, ignore errors

		reason := req.Text
		if reason == "" {
			reason = "jira-wake"
		}

		// Dedup: reject if a heartbeat/cron/evolve session is already running
		sm.mu.RLock()
		for _, s := range sm.sessions {
			if (s.Category == CategoryHeartbeat || s.Category == CategoryCron || s.Category == CategoryEvolve) &&
				s.process != nil && s.process.alive() {
				sm.mu.RUnlock()
				fmt.Fprintf(os.Stderr, "[%s] server: wake rejected — %s session %s already running\n",
					appName, s.Category, shortID(s.ID))
				writeJSON(w, http.StatusConflict, map[string]string{
					"error":    fmt.Sprintf("%s session already running", s.Category),
					"session":  s.ID,
					"category": s.Category,
				})
				return
			}
		}
		sm.mu.RUnlock()

		// Default: soul enabled (未然). Caller can explicitly disable.
		soulEnabled := true
		if req.Soul != nil {
			soulEnabled = *req.Soul
		}

		// Build heartbeat task
		hb := heartbeatTask()
		taskMsg := hb
		if reason != "jira-wake" {
			taskMsg = fmt.Sprintf("Context: %s\n\n%s", reason, hb)
		}

		// Soul session lifecycle: compact check → resume or create
		endStaleSoulSessions()

		// Auto-compact: if rounds exceeded, end old session and start fresh
		if shouldCompactSoulSession(agentName, soulSessionMaxRounds) {
			fmt.Fprintf(os.Stderr, "[%s] server: soul session hit %d rounds, compacting (daily notes carry context forward)\n",
				appName, soulSessionMaxRounds)
			endSoulSession(agentName, fmt.Sprintf("context compaction (rounds >= %d)", soulSessionMaxRounds))
		}

		claudeResumeID := getActiveSoulSession(agentName)
		soulSessionID := getActiveSoulSessionID(agentName)
		if claudeResumeID != "" {
			fmt.Fprintf(os.Stderr, "[%s] server: wake resuming soul session (claude %s)\n", appName, shortID(claudeResumeID))
		} else {
			// No active soul session — create a new daily one
			soulSessionID = createSoulSession(agentName, "daily", 2.0)
		}

		// Apply model: request > defaultModel > empty (upstream default)
		wakeModel := req.Model
		if wakeModel == "" && defaultModel != "" {
			wakeModel = defaultModel
		}

		sessName := fmt.Sprintf("heartbeat-%s", time.Now().Format("0102-1504"))
		sess, err := sm.createSessionWithOpts(sessionCreateOpts{
			Name:     sessName,
			Project:  workspace,
			Model:    wakeModel,
			Soul:     soulEnabled,
			Category: CategoryHeartbeat,
			Tags:     []string{"auto"},
			ResumeID: claudeResumeID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] server: wake failed to create session: %v\n", appName, err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}

		// Link or touch soul session once Claude session ID is available
		if soulSessionID > 0 {
			go func(soulID int64, s *serverSession) {
				deadline := time.Now().Add(30 * time.Second)
				for time.Now().Before(deadline) {
					s.mu.Lock()
					cid := s.ClaudeSID
					s.mu.Unlock()
					if cid != "" {
						linkSoulSession(soulID, cid)
						return
					}
					time.Sleep(500 * time.Millisecond)
				}
			}(soulSessionID, sess)
		}

		// Send heartbeat task as initial message
		go func() {
			time.Sleep(500 * time.Millisecond)
			if err := sess.process.sendMessage(taskMsg); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] server: wake failed to send task: %v\n", appName, err)
			}
		}()

		// Track rounds for auto-compact
		incrementSoulSessionRounds(agentName)

		soulLabel := "with soul"
		if !soulEnabled {
			soulLabel = "no soul"
		}
		resumeLabel := ""
		if claudeResumeID != "" {
			resumeLabel = fmt.Sprintf(" [resume %s]", shortID(claudeResumeID))
		}
		fmt.Fprintf(os.Stderr, "[%s] server: wake triggered: %s (%s, session %s%s)\n", appName, reason, soulLabel, shortID(sess.ID), resumeLabel)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"session_id": sess.ID,
			"reason":     reason,
			"soul":       soulEnabled,
			"resumed":    claudeResumeID != "",
		})
	})

	// Spawn API — dispatch a task to any agent
	// Body: {"agent": "intern", "task": "处理 #815", "wait": false}
	// No auth required — Jira and 未然 both call this
	mux.HandleFunc("POST /api/spawn", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Agent string `json:"agent"`
			Task  string `json:"task"`
			Wait  bool   `json:"wait"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Agent == "" || req.Task == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent and task are required"})
			return
		}

		agents, err := loadAgents()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "load agents: " + err.Error()})
			return
		}

		agent := findAgent(agents, req.Agent)
		if agent == nil {
			available := []string{}
			for _, a := range agents {
				if a.ID != "main" {
					available = append(available, a.ID)
				}
			}
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error":     "agent not found: " + req.Agent,
				"available": available,
			})
			return
		}

		if agent.ID == "main" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "use /api/wake for main agent"})
			return
		}

		// Per-agent mutual exclusion
		if running := agentRunningSpawn(agent.ID); running != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      fmt.Sprintf("agent %s already has a running spawn", agent.ID),
				"spawn_id":   running.id,
				"task":       running.task,
				"started_at": running.started,
			})
			return
		}

		// Build prompt and spawn
		promptContent := buildAgentPrompt(agent)
		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("%s-spawn-%s-", appName, agent.ID))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create temp dir: " + err.Error()})
			return
		}
		promptFile := filepath.Join(tmpDir, "prompt.md")
		os.WriteFile(promptFile, []byte(promptContent), 0600)

		sessionName := fmt.Sprintf("spawn-%s-%s", agent.ID, time.Now().Format("0102-1504"))
		spawnID := recordSpawnStart(agent, req.Task, sessionName, tmpDir)

		claudeArgs := []string{
			"--append-system-prompt-file", promptFile,
			"--dangerously-skip-permissions",
			"--name", sessionName,
			"-p", req.Task,
		}
		if agent.Model != "" {
			claudeArgs = append(claudeArgs, "--model", agent.Model)
		}

		cmd := exec.Command(claudeBin, claudeArgs...)
		cmd.Dir = agent.Workspace
		cmd.Env = injectProxyEnv(os.Environ())

		// Set JIRA_TOKEN
		jiraTokenFile := filepath.Join(agent.Workspace, ".jira-token")
		if data, err := os.ReadFile(jiraTokenFile); err == nil {
			if token := strings.TrimSpace(string(data)); token != "" {
				cmd.Env = append(cmd.Env, "JIRA_TOKEN="+token)
			}
		}

		// Async: double-fork via wrapper script
		logFile := filepath.Join(tmpDir, "output.log")
		pidFile := filepath.Join(tmpDir, "pid")
		exitFile := filepath.Join(tmpDir, "exit")
		notifyBin, _ := exec.LookPath(os.Args[0])
		if notifyBin == "" {
			notifyBin = os.Args[0]
		}

		wrapperScript := fmt.Sprintf("#!/bin/sh\necho $$ > %q\n%s > %q 2>&1\nEXIT_CODE=$?\necho \"$EXIT_CODE\" > %q\nDURATION=$SECONDS\n%s spawn finish %d $EXIT_CODE $DURATION %q 2>/dev/null\nif [ \"$EXIT_CODE\" -eq 0 ]; then\n  %s notify \"✅ spawn %s 完成 (${DURATION}s)\\nsession: %s\"\nelse\n  %s notify \"❌ spawn %s 失败 (exit=$EXIT_CODE, ${DURATION}s)\\nsession: %s\\nlog: %s\"\nfi\n",
			pidFile,
			shellQuoteArgs(cmd.Path, cmd.Args[1:]...), logFile,
			exitFile,
			notifyBin, spawnID, logFile,
			notifyBin, agent.Name, sessionName,
			notifyBin, agent.Name, sessionName, logFile,
		)

		wrapperPath := filepath.Join(tmpDir, "run.sh")
		os.WriteFile(wrapperPath, []byte(wrapperScript), 0700)

		bgCmd := exec.Command("/bin/sh", wrapperPath)
		bgCmd.Dir = agent.Workspace
		bgCmd.Env = cmd.Env
		bgCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		bgCmd.Stdin = nil
		bgCmd.Stdout = nil
		bgCmd.Stderr = nil

		if err := bgCmd.Start(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "spawn failed: " + err.Error()})
			return
		}
		bgCmd.Process.Release()

		fmt.Fprintf(os.Stderr, "[%s] server: spawned %s (%s) for task: %s\n",
			appName, agent.Name, agent.ID, truncate(req.Task, 80))

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"spawn_id": spawnID,
			"agent":    agent.ID,
			"session":  sessionName,
			"log":      logFile,
		})
	})

	// WebSocket endpoint for bidirectional real-time sync
	mux.HandleFunc("GET /api/ws", hub.serveWS)

	// Serve uploaded files (authed)
	uploadsDir := filepath.Join(workspace, "uploads")
	mux.HandleFunc("GET /uploads/", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		// Strip /uploads/ prefix and serve from uploadsDir
		name := strings.TrimPrefix(r.URL.Path, "/uploads/")
		if name == "" || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		filePath := filepath.Join(uploadsDir, name)
		http.ServeFile(w, r, filePath)
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

	// Start Telegram bot if enabled
	var tgBridge *telegramBridge
	if cfg.Telegram.Enabled {
		tgToken := getTelegramToken()
		if tgToken == "" {
			fmt.Fprintf(os.Stderr, "[%s] server: telegram enabled but no bot token found\n", appName)
		} else {
			allowedIDs := cfg.Telegram.AllowedChatIDs
			if len(allowedIDs) == 0 && tgChatID != "" {
				allowedIDs = []string{tgChatID} // fallback to global chat ID
			}
			tgBridge = newTelegramBridge(tgToken, allowedIDs, sm, hub)
			tgBridge.start()
		}
	}

	// Register proxy usage API endpoint
	registerProxyAPI(mux, cfg.Token)

	// Start Anthropic API proxy if enabled
	if cfg.Proxy.Enabled {
		startProxyServer(cfg.Proxy)
	}

	// Start server
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE needs no write timeout
		IdleTimeout:  120 * time.Second,
	}

	// Parent context for background goroutines (session lifecycle watcher etc).
	// Cancelled in the graceful shutdown handler below so watchers exit cleanly.
	bgCtx, bgCancel := context.WithCancel(context.Background())

	// Session lifecycle watcher: expires idle soul sessions + daily reset.
	// Runs as a background goroutine inside the server process.
	resetPolicy := loadResetPolicyFromConfig(cfg)
	go sessionLifecycleWatcher(bgCtx, resetPolicy, 5*time.Minute)

	// Graceful shutdown on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\n[%s] server: received %v, shutting down...\n", appName, sig)

		// Stop background goroutines (lifecycle watcher etc)
		bgCancel()

		// Stop Telegram bot first (stops receiving new messages)
		if tgBridge != nil {
			tgBridge.shutdown()
		}

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
