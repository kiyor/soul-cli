package main

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Anthropic API Proxy ──
// Transparent reverse proxy that captures rate-limit headers and token usage
// from Anthropic API responses. Claude Code sets ANTHROPIC_BASE_URL to this proxy.

// proxyConfig holds proxy settings from config.json "server.proxy".
type proxyConfig struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`     // default 9091
	Upstream string `json:"upstream"` // default https://api.anthropic.com
}

func defaultProxyConfig() proxyConfig {
	return proxyConfig{
		Enabled:  false,
		Port:     9091,
		Upstream: "https://api.anthropic.com",
	}
}

// RateLimitSnapshot holds captured rate-limit info from API response headers.
type RateLimitSnapshot struct {
	FiveHourUtil  float64   `json:"five_hour_utilization"`  // 0-1
	FiveHourReset int64     `json:"five_hour_resets_at"`    // unix epoch seconds
	SevenDayUtil  float64   `json:"seven_day_utilization"`  // 0-1
	SevenDayReset int64     `json:"seven_day_resets_at"`    // unix epoch seconds
	Status        string    `json:"status"`                 // allowed / allowed_warning / rejected
	Fallback      string    `json:"fallback"`               // available / ""
	IsOverage     bool      `json:"is_overage"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// TokenAccumulator tracks cumulative token usage.
type TokenAccumulator struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	RequestCount        int64 `json:"request_count"`
}

// DailyTokenUsage tracks per-day per-model token usage.
type DailyTokenUsage struct {
	Date   string                       `json:"date"` // YYYY-MM-DD
	Models map[string]*TokenAccumulator `json:"models"`
}

// proxyState is the shared in-memory state for the proxy.
type proxyState struct {
	mu        sync.RWMutex
	RateLimit RateLimitSnapshot          `json:"rate_limit"`
	Today     DailyTokenUsage            `json:"today"`
	Total     TokenAccumulator           `json:"total"` // all-time since server start
}

var pState = &proxyState{}

func (ps *proxyState) updateRateLimit(snap RateLimitSnapshot) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.RateLimit = snap
}

func (ps *proxyState) addTokens(model string, input, output, cacheRead, cacheCreate int64) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if ps.Today.Date != today {
		// Day rolled over, reset
		ps.Today = DailyTokenUsage{Date: today, Models: make(map[string]*TokenAccumulator)}
	}
	if ps.Today.Models == nil {
		ps.Today.Models = make(map[string]*TokenAccumulator)
	}

	acc, ok := ps.Today.Models[model]
	if !ok {
		acc = &TokenAccumulator{}
		ps.Today.Models[model] = acc
	}
	// Plain adds under write lock — no need for atomic since mu.Lock is held
	acc.InputTokens += input
	acc.OutputTokens += output
	acc.CacheReadTokens += cacheRead
	acc.CacheCreationTokens += cacheCreate
	acc.RequestCount++

	// Total
	ps.Total.InputTokens += input
	ps.Total.OutputTokens += output
	ps.Total.CacheReadTokens += cacheRead
	ps.Total.CacheCreationTokens += cacheCreate
	ps.Total.RequestCount++
}

func (ps *proxyState) snapshot() map[string]any {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return map[string]any{
		"rate_limit": ps.RateLimit,
		"today":      ps.Today,
		"total":      ps.Total,
	}
}

// ── Rate-limit header capture ──

// captureRateLimitHeaders extracts anthropic-ratelimit-unified-* headers.
func captureRateLimitHeaders(h http.Header) RateLimitSnapshot {
	snap := RateLimitSnapshot{
		UpdatedAt: time.Now(),
	}
	snap.Status = h.Get("anthropic-ratelimit-unified-status")
	snap.Fallback = h.Get("anthropic-ratelimit-unified-fallback")

	if v := h.Get("anthropic-ratelimit-unified-5h-utilization"); v != "" {
		snap.FiveHourUtil, _ = strconv.ParseFloat(v, 64)
	}
	if v := h.Get("anthropic-ratelimit-unified-5h-reset"); v != "" {
		snap.FiveHourReset, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := h.Get("anthropic-ratelimit-unified-7d-utilization"); v != "" {
		snap.SevenDayUtil, _ = strconv.ParseFloat(v, 64)
	}
	if v := h.Get("anthropic-ratelimit-unified-7d-reset"); v != "" {
		snap.SevenDayReset, _ = strconv.ParseInt(v, 10, 64)
	}

	overageStatus := h.Get("anthropic-ratelimit-unified-overage-status")
	if snap.Status == "rejected" && (overageStatus == "allowed" || overageStatus == "allowed_warning") {
		snap.IsOverage = true
	}

	return snap
}

// ── Request/response body extraction ──

// proxyRequestInfo collects data from a single proxied request for logging.
type proxyRequestInfo struct {
	StartTime    time.Time
	SessionID    string // weiran session ID, extracted from URL path prefix /s/{id}/
	Model        string
	Stream       bool
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheCreate  int64
	StopReason   string
	ToolsCount   int
	ToolCalls    string
	ThinkingMode string
	Temperature  string
	UserAgent    string
	RequestID    string
	Status       int
	IsError      bool
	ErrorDetail  string
}

// extractRequestMeta pulls model, temperature, thinking mode, tools count from request body.
func extractRequestMeta(body []byte) (model, thinkingMode, temperature string, toolsCount int) {
	var req struct {
		Model    string `json:"model"`
		Thinking struct {
			Type   string `json:"type"`
			Budget int    `json:"budget_tokens"`
		} `json:"thinking"`
		Temperature *float64 `json:"temperature"`
		Tools       []any    `json:"tools"`
	}
	if json.Unmarshal(body, &req) == nil {
		model = req.Model
		toolsCount = len(req.Tools)
		if req.Thinking.Type != "" {
			thinkingMode = req.Thinking.Type
			if req.Thinking.Type == "enabled" && req.Thinking.Budget > 0 {
				thinkingMode = fmt.Sprintf("enabled:%d", req.Thinking.Budget)
			}
		}
		if req.Temperature != nil {
			temperature = fmt.Sprintf("%.2f", *req.Temperature)
		}
	}
	return
}

// extractUsageFromBody tries to pull "usage" from a non-streaming JSON response body.
// Returns model, input, output, cacheRead, cacheCreate, stopReason, toolCalls.
func extractUsageFromBody(body []byte) (model string, input, output, cacheRead, cacheCreate int64, stopReason, toolCalls, errDetail string) {
	var msg struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &msg) == nil {
		model = msg.Model
		stopReason = msg.StopReason
		input = msg.Usage.InputTokens
		output = msg.Usage.OutputTokens
		cacheRead = msg.Usage.CacheReadInputTokens
		cacheCreate = msg.Usage.CacheCreationInputTokens

		// Extract tool_use calls from content
		var tools []string
		for _, c := range msg.Content {
			if c.Type == "tool_use" && c.Name != "" {
				tools = append(tools, c.Name)
			}
		}
		if len(tools) > 0 {
			toolCalls = strings.Join(tools, ",")
		}

		if msg.Error.Type != "" {
			errDetail = msg.Error.Type + ": " + msg.Error.Message
		}
	}
	return
}

// extractUsageFromSSE scans SSE stream bytes for message_start/message_delta usage.
// Forward iteration (matching event arrival order) + fallback estimation for partial streams.
func extractUsageFromSSE(body []byte) (model string, input, output, cacheRead, cacheCreate int64, stopReason, toolCalls string) {
	lines := bytes.Split(body, []byte("\n"))
	var tools []string
	var totalTextBytes int

	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := line[6:]

		// Quick filter: only JSON-parse lines that might contain what we need
		dataStr := string(data)
		hasUsage := strings.Contains(dataStr, "usage")
		hasContentBlock := strings.Contains(dataStr, "content_block")

		if !hasUsage && !hasContentBlock && !strings.Contains(dataStr, "text_delta") {
			continue
		}

		var evt struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens              int64 `json:"input_tokens"`
					OutputTokens             int64 `json:"output_tokens"`
					CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Delta struct {
				Type       string `json:"type"`
				StopReason string `json:"stop_reason"`
				Name       string `json:"name"`
				Text       string `json:"text"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &evt) != nil {
			continue
		}

		switch evt.Type {
		case "message_start":
			if evt.Message.Model != "" {
				model = evt.Message.Model
			}
			// Unconditionally capture — input_tokens can be 0 when all cached
			u := evt.Message.Usage
			input = u.InputTokens
			cacheRead = u.CacheReadInputTokens
			cacheCreate = u.CacheCreationInputTokens

		case "message_delta":
			output = evt.Usage.OutputTokens
			if evt.Delta.StopReason != "" {
				stopReason = evt.Delta.StopReason
			}

		case "content_block_start":
			if evt.Delta.Type == "tool_use" && evt.Delta.Name != "" {
				tools = append(tools, evt.Delta.Name)
			}

		case "content_block_delta":
			// Accumulate text for fallback output estimation
			if evt.Delta.Text != "" {
				totalTextBytes += len(evt.Delta.Text)
			}
		}
	}

	// Fallback: if message_delta was missing (client disconnect / partial stream),
	// estimate output tokens from accumulated text_delta bytes (~4 bytes/token)
	if output == 0 && totalTextBytes > 0 {
		output = int64(totalTextBytes / 4)
		if output == 0 {
			output = 1
		}
	}

	if len(tools) > 0 {
		toolCalls = strings.Join(tools, ",")
	}
	return
}

// ── Pricing (Pool-grade) ──

// modelPricing maps model names to [input, output] prices per 1M tokens (USD).
// Synced from claude-pool main.go — keep in sync when Anthropic updates pricing.
// MiniMax: CNY ÷ 7. GLM (Z.AI): already USD. Source dates: 2026-04-10.
var modelPricing = map[string][2]float64{
	// Claude 4.5/4.6 (short names used by Claude Code)
	"claude-opus-4-5":            {5.0, 25.0},
	"claude-opus-4-6":            {5.0, 25.0},
	"opus-4-5":                   {5.0, 25.0},
	"opus-4-6":                   {5.0, 25.0},
	"claude-sonnet-4-5":          {3.0, 15.0},
	"claude-sonnet-4-6":          {3.0, 15.0},
	"sonnet-4-5":                 {3.0, 15.0},
	"sonnet-4-6":                 {3.0, 15.0},
	"claude-haiku-4-5":           {1.0, 5.0},
	"claude-haiku-4-6":           {1.0, 5.0},
	"haiku-4-5":                  {1.0, 5.0},
	"haiku-4-6":                  {1.0, 5.0},
	// Full model IDs
	"claude-sonnet-4-20250514":   {3.0, 15.0},
	"claude-opus-4-20250514":     {5.0, 25.0},
	"claude-haiku-4-5-20251001":  {1.0, 5.0},
	// Legacy models (3.x)
	"claude-3-5-sonnet-20241022": {3.0, 15.0},
	"claude-3-5-haiku-20241022":  {0.8, 4.0},
	"claude-3-opus-20240229":     {15.0, 75.0},
	"claude-3-haiku-20240307":    {0.25, 1.25},

	// MiniMax (platform.minimaxi.com/docs/guides/pricing-paygo, CNY ÷ 7)
	"MiniMax-M2.7":             {0.30, 1.20}, // ¥2.1/¥8.4
	"MiniMax-M2.7-highspeed":   {0.60, 2.40}, // ¥4.2/¥16.8
	"MiniMax-M2.5":             {0.30, 1.20}, // ¥2.1/¥8.4
	"MiniMax-M2.5-highspeed":   {0.60, 2.40}, // ¥4.2/¥16.8
	"M2-her":                   {0.30, 1.20}, // ¥2.1/¥8.4

	// GLM / Z.AI (docs.z.ai/guides/overview/pricing, USD)
	"glm-5.1":              {1.40, 4.40},
	"glm-5":                {1.00, 3.20},
	"glm-5-turbo":          {1.20, 4.00},
	"glm-4.7":              {0.60, 2.20},
	"glm-4.7-flashx":       {0.07, 0.40},
	"glm-4.7-flash":        {0.00, 0.00}, // free
	"glm-4.6":              {0.60, 2.20},
	"glm-4.5":              {0.60, 2.20},
	"glm-4.5-x":            {2.20, 8.90},
	"glm-4.5-air":          {0.20, 1.10},
	"glm-4.5-airx":         {1.10, 4.50},
	"glm-4.5-flash":        {0.00, 0.00}, // free
	"glm-4-32b-0414-128k":  {0.10, 0.10},
}

// calcCost computes USD cost with cache-aware pricing.
// Anthropic API returns input_tokens as NET (excludes cache), so the three token
// types are additive: input_tokens + cache_read + cache_create = total input.
// Cache read = 10% of input rate (90% discount). Cache create = 125% of input rate (25% surcharge).
func calcCost(model string, inputTokens, outputTokens, cacheRead, cacheCreate int64) float64 {
	p, ok := modelPricing[model]
	if !ok {
		// Try prefix matching for versioned models (e.g. "claude-sonnet-4-20260514")
		for k, v := range modelPricing {
			if strings.HasPrefix(model, k) {
				p = v
				ok = true
				break
			}
		}
		if !ok {
			// Conservative fallback: use Sonnet pricing. Models with free tiers
			// (GLM flash, etc.) should be added to modelPricing explicitly.
			p = [2]float64{3.0, 15.0}
		}
	}
	inputRate := p[0] / 1_000_000
	outputRate := p[1] / 1_000_000
	cacheReadRate := inputRate * 0.1   // 90% discount
	cacheCreateRate := inputRate * 1.25 // 25% surcharge
	// input_tokens is already net (non-cached), no subtraction needed
	return float64(inputTokens)*inputRate + float64(outputTokens)*outputRate + float64(cacheRead)*cacheReadRate + float64(cacheCreate)*cacheCreateRate
}

// ── SQLite request log ──

var proxyDB *sql.DB

func initProxyDB() {
	dbFile := appDir + "/proxy.db"
	var err error
	proxyDB, err = sql.Open("sqlite", dbFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] proxy-db: failed to open %s: %v\n", appName, dbFile, err)
		return
	}
	if _, err := proxyDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] proxy-db: WAL mode failed: %v\n", appName, err)
	}
	if _, err := proxyDB.Exec("PRAGMA busy_timeout=5000"); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] proxy-db: busy_timeout failed: %v\n", appName, err)
	}

	_, err = proxyDB.Exec(`CREATE TABLE IF NOT EXISTS proxy_requests (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		time          TEXT NOT NULL,
		session_id    TEXT NOT NULL DEFAULT '',
		model         TEXT NOT NULL DEFAULT '',
		status        INTEGER NOT NULL DEFAULT 0,
		is_error      INTEGER NOT NULL DEFAULT 0,
		stream        INTEGER NOT NULL DEFAULT 0,
		input_tokens  INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read    INTEGER NOT NULL DEFAULT 0,
		cache_create  INTEGER NOT NULL DEFAULT 0,
		cost_usd      REAL NOT NULL DEFAULT 0,
		duration_ms   INTEGER NOT NULL DEFAULT 0,
		stop_reason   TEXT NOT NULL DEFAULT '',
		tools_count   INTEGER NOT NULL DEFAULT 0,
		tool_calls    TEXT NOT NULL DEFAULT '',
		thinking_mode TEXT NOT NULL DEFAULT '',
		temperature   TEXT NOT NULL DEFAULT '',
		user_agent    TEXT NOT NULL DEFAULT '',
		error_detail  TEXT NOT NULL DEFAULT '',
		request_id    TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] proxy-db: create table failed: %v\n", appName, err)
	}

	// Migrations (idempotent — ignore "duplicate column" errors)
	proxyDB.Exec("ALTER TABLE proxy_requests ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0")
	proxyDB.Exec("ALTER TABLE proxy_requests ADD COLUMN session_id TEXT NOT NULL DEFAULT ''")

	// Backfill cost_usd for existing records that have tokens but no cost
	go func() {
		rows, err := proxyDB.Query("SELECT id, model, input_tokens, output_tokens, cache_read, cache_create FROM proxy_requests WHERE cost_usd = 0 AND (input_tokens > 0 OR output_tokens > 0)")
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id int
			var model string
			var inTok, outTok, cacheR, cacheC int64
			rows.Scan(&id, &model, &inTok, &outTok, &cacheR, &cacheC)
			cost := calcCost(model, inTok, outTok, cacheR, cacheC)
			proxyDB.Exec("UPDATE proxy_requests SET cost_usd = ? WHERE id = ?", cost, id)
		}
	}()

	// Indexes
	proxyDB.Exec("CREATE INDEX IF NOT EXISTS idx_proxy_time ON proxy_requests(time)")
	proxyDB.Exec("CREATE INDEX IF NOT EXISTS idx_proxy_model ON proxy_requests(model)")
	proxyDB.Exec("CREATE INDEX IF NOT EXISTS idx_proxy_session ON proxy_requests(session_id)")

	// Auto-cleanup: delete records older than 30 days
	go func() {
		cutoff := time.Now().AddDate(0, 0, -30).Format(time.RFC3339)
		proxyDB.Exec("DELETE FROM proxy_requests WHERE time < ?", cutoff)
	}()

	fmt.Fprintf(os.Stderr, "[%s] proxy-db: initialized %s\n", appName, dbFile)
}

func saveProxyRequest(info proxyRequestInfo) {
	if proxyDB == nil {
		return
	}
	go func() {
		isErr := 0
		if info.IsError {
			isErr = 1
		}
		isStream := 0
		if info.Stream {
			isStream = 1
		}
		costUSD := calcCost(info.Model, info.InputTokens, info.OutputTokens, info.CacheRead, info.CacheCreate)
		_, err := proxyDB.Exec(`INSERT INTO proxy_requests
			(time, session_id, model, status, is_error, stream, input_tokens, output_tokens,
			 cache_read, cache_create, cost_usd, duration_ms, stop_reason, tools_count,
			 tool_calls, thinking_mode, temperature, user_agent, error_detail, request_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			info.StartTime.Format(time.RFC3339),
			info.SessionID,
			info.Model, info.Status, isErr, isStream,
			info.InputTokens, info.OutputTokens, info.CacheRead, info.CacheCreate,
			costUSD,
			time.Since(info.StartTime).Milliseconds(),
			info.StopReason, info.ToolsCount, info.ToolCalls,
			info.ThinkingMode, info.Temperature, info.UserAgent,
			info.ErrorDetail, info.RequestID,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] proxy-db: insert failed: %v\n", appName, err)
		}
	}()
}

// ── Proxy server ──

// startProxyServer starts the transparent Anthropic API proxy.
func startProxyServer(cfg proxyConfig) {
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] proxy: invalid upstream URL %q: %v\n", appName, cfg.Upstream, err)
		return
	}

	// Initialize proxy request log DB
	initProxyDB()

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Force no compression so we can parse SSE as plaintext.
			// Must set explicitly — Go's Transport adds "gzip" if header is absent.
			req.Header.Set("Accept-Encoding", "identity")

			// Extract session ID from path prefix: /s/{session_id}/v1/messages → /v1/messages
			if strings.HasPrefix(req.URL.Path, "/s/") {
				rest := req.URL.Path[3:] // strip "/s/"
				if idx := strings.IndexByte(rest, '/'); idx > 0 {
					req.Header.Set("X-Proxy-Session-ID", rest[:idx])
					req.URL.Path = rest[idx:] // e.g. /v1/messages
					req.URL.RawPath = ""
				}
			}

			// Stash request metadata in context-like header for ModifyResponse
			// We use a custom header prefix that gets stripped before forwarding
			req.Header.Set("X-Proxy-Start", fmt.Sprintf("%d", time.Now().UnixNano()))
			req.Header.Set("X-Proxy-UA", req.Header.Get("User-Agent"))

			// Extract request body metadata (model, thinking, temperature, tools)
			if req.Body != nil && req.Method == "POST" {
				body, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err == nil {
					model, thinking, temp, toolsCount := extractRequestMeta(body)
					req.Header.Set("X-Proxy-Req-Model", model)
					req.Header.Set("X-Proxy-Req-Thinking", thinking)
					req.Header.Set("X-Proxy-Req-Temp", temp)
					req.Header.Set("X-Proxy-Req-Tools", strconv.Itoa(toolsCount))
					isStream := strings.Contains(string(body), `"stream":true`) || strings.Contains(string(body), `"stream": true`)
					if isStream {
						req.Header.Set("X-Proxy-Stream", "1")
					}
					req.Body = io.NopCloser(bytes.NewReader(body))
					req.ContentLength = int64(len(body))
				}
			}

			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host

			// NOTE: X-Proxy-* headers are kept on the request object so that
			// ModifyResponse can read them. They are stripped below in Transport
			// before they actually reach the upstream server.
		},
		Transport: &headerStrippingTransport{base: http.DefaultTransport},
		ModifyResponse: func(resp *http.Response) error {
			req := resp.Request

			// Capture rate-limit headers on every response
			snap := captureRateLimitHeaders(resp.Header)
			if snap.Status != "" || snap.FiveHourUtil > 0 || snap.SevenDayUtil > 0 {
				pState.updateRateLimit(snap)
			}

			// Build request info for logging
			info := proxyRequestInfo{
				StartTime:    time.Now(),
				SessionID:    req.Header.Get("X-Proxy-Session-ID"),
				Status:       resp.StatusCode,
				IsError:      resp.StatusCode >= 400,
				UserAgent:    req.Header.Get("X-Proxy-UA"),
				RequestID:    resp.Header.Get("request-id"),
				Model:        req.Header.Get("X-Proxy-Req-Model"),
				ThinkingMode: req.Header.Get("X-Proxy-Req-Thinking"),
				Temperature:  req.Header.Get("X-Proxy-Req-Temp"),
				Stream:       req.Header.Get("X-Proxy-Stream") == "1",
			}
			if tc := req.Header.Get("X-Proxy-Req-Tools"); tc != "" {
				info.ToolsCount, _ = strconv.Atoi(tc)
			}
			if startNano := req.Header.Get("X-Proxy-Start"); startNano != "" {
				if ns, err := strconv.ParseInt(startNano, 10, 64); err == nil {
					info.StartTime = time.Unix(0, ns)
				}
			}

			ct := resp.Header.Get("Content-Type")
			isStream := strings.Contains(ct, "text/event-stream")
			info.Stream = isStream || info.Stream

			if !isStream && strings.Contains(ct, "application/json") {
				// Read body, extract usage, then restore it
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err == nil {
					model, in, out, cr, cc, stopReason, toolCalls, errDetail := extractUsageFromBody(body)
					if in > 0 || out > 0 {
						pState.addTokens(model, in, out, cr, cc)
					}
					if model != "" {
						info.Model = model
					}
					info.InputTokens = in
					info.OutputTokens = out
					info.CacheRead = cr
					info.CacheCreate = cc
					info.StopReason = stopReason
					info.ToolCalls = toolCalls
					info.ErrorDetail = errDetail
					resp.Body = io.NopCloser(bytes.NewReader(body))
					resp.ContentLength = int64(len(body))
				}
				saveProxyRequest(info)
			}

			// For streaming, we use a wrapping reader
			if isStream {
				resp.Body = &streamCaptureReader{ReadCloser: resp.Body, info: info}
			}

			return nil
		},
	}

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      proxy,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming needs no write timeout
		IdleTimeout:  120 * time.Second,
	}

	activeProxyPort = cfg.Port
	fmt.Fprintf(os.Stderr, "[%s] proxy: listening on %s → %s\n", appName, addr, cfg.Upstream)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[%s] proxy: fatal: %v\n", appName, err)
		}
	}()
}

// headerStrippingTransport strips internal X-Proxy-* headers before sending
// the request upstream, preventing metadata leakage to the Anthropic API.
// The headers remain on the *http.Request object for ModifyResponse to read.
type headerStrippingTransport struct {
	base http.RoundTripper
}

func (t *headerStrippingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone headers so we don't modify the original (ModifyResponse needs them)
	h2 := req.Header.Clone()
	for key := range h2 {
		if strings.HasPrefix(key, "X-Proxy-") {
			h2.Del(key)
		}
	}
	req2 := req.Clone(req.Context())
	req2.Header = h2
	return t.base.RoundTrip(req2)
}

// streamCaptureReader wraps an SSE stream body, buffering it to extract usage on Close.
type streamCaptureReader struct {
	io.ReadCloser
	buf  bytes.Buffer
	info proxyRequestInfo
}

func (r *streamCaptureReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.buf.Write(p[:n])
	}
	if err == io.EOF {
		// Stream finished, extract usage from buffered SSE data
		bufBytes := r.buf.Bytes()

		// Decompress gzip if needed (Anthropic API may return gzip-encoded SSE)
		if len(bufBytes) >= 2 && bufBytes[0] == 0x1f && bufBytes[1] == 0x8b {
			if gr, gzErr := gzip.NewReader(bytes.NewReader(bufBytes)); gzErr == nil {
				if decoded, readErr := io.ReadAll(gr); readErr == nil {
					bufBytes = decoded
				}
				gr.Close()
			}
		}

		model, in, out, cr, cc, stopReason, toolCalls := extractUsageFromSSE(bufBytes)
		if in > 0 || out > 0 || cr > 0 || cc > 0 {
			pState.addTokens(model, in, out, cr, cc)
		}
		if in == 0 && out == 0 && cr == 0 && len(bufBytes) > 0 {
			snippet := string(bufBytes)
			if len(snippet) > 300 {
				snippet = snippet[:300]
			}
			fmt.Fprintf(os.Stderr, "[%s] proxy-sse: zero tokens from %d bytes (model=%s) first300=%q\n", appName, len(bufBytes), model, snippet)
		}
		// Log to DB
		r.info.InputTokens = in
		r.info.OutputTokens = out
		r.info.CacheRead = cr
		r.info.CacheCreate = cc
		r.info.StopReason = stopReason
		r.info.ToolCalls = toolCalls
		if model != "" {
			r.info.Model = model
		}
		saveProxyRequest(r.info)
	}
	return n, err
}

// ── API Endpoints ──

// registerProxyAPI adds proxy-related endpoints to the main server mux.
func registerProxyAPI(mux *http.ServeMux, token string) {
	// GET /api/proxy/usage — in-memory usage snapshot (existing)
	mux.HandleFunc("GET /api/proxy/usage", authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, pState.snapshot())
	}))

	// GET /api/proxy/logs — query request log from SQLite
	mux.HandleFunc("GET /api/proxy/logs", authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		if proxyDB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "proxy db not initialized"})
			return
		}
		q := r.URL.Query()
		limit := intParam(q, "limit", 50)
		offset := intParam(q, "offset", 0)
		if limit > 500 {
			limit = 500
		}

		where := []string{"1=1"}
		args := []any{}

		if sid := q.Get("session_id"); sid != "" {
			where = append(where, "session_id = ?")
			args = append(args, sid)
		}
		if m := q.Get("model"); m != "" {
			where = append(where, "model LIKE ?")
			args = append(args, "%"+m+"%")
		}
		if q.Get("error") == "true" {
			where = append(where, "is_error = 1")
		}
		if since := q.Get("since"); since != "" {
			where = append(where, "time >= ?")
			args = append(args, since)
		}
		if until := q.Get("until"); until != "" {
			where = append(where, "time <= ?")
			args = append(args, until)
		}
		if st := q.Get("status"); st != "" {
			switch st {
			case "2xx":
				where = append(where, "status >= 200 AND status < 300")
			case "4xx":
				where = append(where, "status >= 400 AND status < 500")
			case "5xx":
				where = append(where, "status >= 500 AND status < 600")
			}
		}

		// Sort support
		sortCol := "id"
		sortDir := "DESC"
		if sc := q.Get("sort"); sc != "" {
			allowed := map[string]bool{"time": true, "model": true, "status": true, "input_tokens": true, "output_tokens": true, "cost_usd": true, "duration_ms": true}
			if allowed[sc] {
				sortCol = sc
			}
		}
		if q.Get("dir") == "asc" {
			sortDir = "ASC"
		}

		query := fmt.Sprintf(
			"SELECT id,time,session_id,model,status,is_error,stream,input_tokens,output_tokens,cache_read,cache_create,cost_usd,duration_ms,stop_reason,tools_count,tool_calls,thinking_mode,temperature,user_agent,error_detail,request_id FROM proxy_requests WHERE %s ORDER BY %s %s LIMIT ? OFFSET ?",
			strings.Join(where, " AND "), sortCol, sortDir,
		)
		args = append(args, limit, offset)

		rows, err := proxyDB.Query(query, args...)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()

		var records []map[string]any
		for rows.Next() {
			var (
				id, status, isErr, stream, inTok, outTok, cacheR, cacheC, durMs, toolsCnt int
				costUSD                                                                    float64
				ts, sessionID, model, stopReason, toolCalls, thinking, temp, ua, errD, reqID string
			)
			rows.Scan(&id, &ts, &sessionID, &model, &status, &isErr, &stream, &inTok, &outTok, &cacheR, &cacheC, &costUSD, &durMs, &stopReason, &toolsCnt, &toolCalls, &thinking, &temp, &ua, &errD, &reqID)
			records = append(records, map[string]any{
				"id": id, "time": ts, "session_id": sessionID, "model": model, "status": status,
				"is_error": isErr == 1, "stream": stream == 1,
				"input_tokens": inTok, "output_tokens": outTok,
				"cache_read": cacheR, "cache_create": cacheC,
				"cost_usd": costUSD,
				"duration_ms": durMs, "stop_reason": stopReason,
				"tools_count": toolsCnt, "tool_calls": toolCalls,
				"thinking_mode": thinking, "temperature": temp,
				"user_agent": ua, "error_detail": errD, "request_id": reqID,
			})
		}
		if records == nil {
			records = []map[string]any{}
		}

		// Get total count + summary stats
		countArgs := args[:len(args)-2] // strip limit and offset
		var total int
		summaryQuery := fmt.Sprintf("SELECT COUNT(*), COALESCE(SUM(cost_usd),0), COALESCE(AVG(duration_ms),0), COALESCE(SUM(CASE WHEN is_error=1 THEN 1 ELSE 0 END),0) FROM proxy_requests WHERE %s", strings.Join(where, " AND "))
		var totalCost, avgDur float64
		var totalErrors int
		proxyDB.QueryRow(summaryQuery, countArgs...).Scan(&total, &totalCost, &avgDur, &totalErrors)

		writeJSON(w, http.StatusOK, map[string]any{
			"records":    records,
			"total":      total,
			"limit":      limit,
			"offset":     offset,
			"total_cost": totalCost,
			"avg_dur_ms": int64(avgDur),
			"errors":     totalErrors,
		})
	}))

	// GET /api/proxy/stats — aggregated statistics
	mux.HandleFunc("GET /api/proxy/stats", authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		if proxyDB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "proxy db not initialized"})
			return
		}
		q := r.URL.Query()
		groupBy := q.Get("group")
		if groupBy == "" {
			groupBy = "hour"
		}
		days := intParam(q, "days", 1)
		if days > 90 {
			days = 90
		}

		since := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)

		var timeFmt string
		switch groupBy {
		case "day":
			timeFmt = "%Y-%m-%d"
		default:
			timeFmt = "%Y-%m-%d %H:00"
		}

		rows, err := proxyDB.Query(fmt.Sprintf(`
			SELECT strftime('%s', time) as period,
				model,
				COUNT(*) as requests,
				SUM(input_tokens) as input_tokens,
				SUM(output_tokens) as output_tokens,
				SUM(cache_read) as cache_read,
				SUM(cache_create) as cache_create,
				SUM(cost_usd) as total_cost,
				SUM(CASE WHEN is_error=1 THEN 1 ELSE 0 END) as errors,
				AVG(duration_ms) as avg_duration_ms
			FROM proxy_requests
			WHERE time >= ?
			GROUP BY period, model
			ORDER BY period DESC
		`, timeFmt), since)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()

		var stats []map[string]any
		for rows.Next() {
			var (
				period, model                                        string
				requests, inTok, outTok, cacheR, cacheC, errors      int64
				totalCost, avgDur                                    float64
			)
			rows.Scan(&period, &model, &requests, &inTok, &outTok, &cacheR, &cacheC, &totalCost, &errors, &avgDur)
			stats = append(stats, map[string]any{
				"period": period, "model": model, "requests": requests,
				"input_tokens": inTok, "output_tokens": outTok,
				"cache_read": cacheR, "cache_create": cacheC,
				"cost_usd": totalCost,
				"errors": errors, "avg_duration_ms": int64(avgDur),
			})
		}
		if stats == nil {
			stats = []map[string]any{}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"group": groupBy,
			"days":  days,
			"stats": stats,
		})
	}))

	// GET /api/proxy/session-cost — aggregated cost for a session (or all sessions)
	mux.HandleFunc("GET /api/proxy/session-cost", authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		if proxyDB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "proxy db not initialized"})
			return
		}
		sid := r.URL.Query().Get("session_id")
		if sid != "" {
			// Single session cost
			var cost float64
			var requests, inTok, outTok, cacheR, cacheC int64
			proxyDB.QueryRow(
				"SELECT COALESCE(SUM(cost_usd),0), COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_create),0) FROM proxy_requests WHERE session_id=?",
				sid,
			).Scan(&cost, &requests, &inTok, &outTok, &cacheR, &cacheC)
			writeJSON(w, http.StatusOK, map[string]any{
				"session_id":    sid,
				"cost_usd":      cost,
				"requests":      requests,
				"input_tokens":  inTok,
				"output_tokens": outTok,
				"cache_read":    cacheR,
				"cache_create":  cacheC,
			})
		} else {
			// All sessions with cost > 0
			rows, err := proxyDB.Query(
				"SELECT session_id, SUM(cost_usd), COUNT(*), SUM(input_tokens), SUM(output_tokens) FROM proxy_requests WHERE session_id != '' GROUP BY session_id ORDER BY SUM(cost_usd) DESC LIMIT 200",
			)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			defer rows.Close()
			var sessions []map[string]any
			for rows.Next() {
				var sid string
				var cost float64
				var reqs, inTok, outTok int64
				rows.Scan(&sid, &cost, &reqs, &inTok, &outTok)
				sessions = append(sessions, map[string]any{
					"session_id": sid, "cost_usd": cost, "requests": reqs,
					"input_tokens": inTok, "output_tokens": outTok,
				})
			}
			if sessions == nil {
				sessions = []map[string]any{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
		}
	}))

	// GET /api/proxy/logs/filters — available filter values
	mux.HandleFunc("GET /api/proxy/logs/filters", authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		if proxyDB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "proxy db not initialized"})
			return
		}
		models := []string{}
		rows, err := proxyDB.Query("SELECT DISTINCT model FROM proxy_requests WHERE model != '' ORDER BY model")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var m string
				rows.Scan(&m)
				models = append(models, m)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": models})
	}))

	// GET /api/proxy/logs/export — CSV export
	mux.HandleFunc("GET /api/proxy/logs/export", authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		if proxyDB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "proxy db not initialized"})
			return
		}
		q := r.URL.Query()
		where := []string{"1=1"}
		args := []any{}
		if m := q.Get("model"); m != "" {
			where = append(where, "model LIKE ?")
			args = append(args, "%"+m+"%")
		}
		if q.Get("error") == "true" {
			where = append(where, "is_error = 1")
		}
		if since := q.Get("since"); since != "" {
			where = append(where, "time >= ?")
			args = append(args, since)
		}
		if until := q.Get("until"); until != "" {
			where = append(where, "time <= ?")
			args = append(args, until)
		}
		if st := q.Get("status"); st != "" {
			switch st {
			case "2xx":
				where = append(where, "status >= 200 AND status < 300")
			case "4xx":
				where = append(where, "status >= 400 AND status < 500")
			case "5xx":
				where = append(where, "status >= 500 AND status < 600")
			}
		}

		query := fmt.Sprintf(
			"SELECT id,time,model,status,is_error,stream,input_tokens,output_tokens,cache_read,cache_create,cost_usd,duration_ms,stop_reason,tools_count,tool_calls,thinking_mode,temperature,user_agent,error_detail,request_id FROM proxy_requests WHERE %s ORDER BY id DESC LIMIT 10000",
			strings.Join(where, " AND "),
		)
		rows, err := proxyDB.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=proxy-logs.csv")
		fmt.Fprintln(w, "id,time,model,status,is_error,stream,input_tokens,output_tokens,cache_read,cache_create,cost_usd,duration_ms,stop_reason,tools_count,tool_calls,thinking_mode,temperature,user_agent,error_detail,request_id")
		for rows.Next() {
			var (
				id, status, isErr, stream, inTok, outTok, cacheR, cacheC, durMs, toolsCnt int
				costUSD                                                                    float64
				ts, model, stopReason, toolCalls, thinking, temp, ua, errD, reqID          string
			)
			rows.Scan(&id, &ts, &model, &status, &isErr, &stream, &inTok, &outTok, &cacheR, &cacheC, &costUSD, &durMs, &stopReason, &toolsCnt, &toolCalls, &thinking, &temp, &ua, &errD, &reqID)
			fmt.Fprintf(w, "%d,%s,%s,%d,%d,%d,%d,%d,%d,%d,%.6f,%d,%s,%d,\"%s\",%s,%s,\"%s\",\"%s\",%s\n",
				id, ts, model, status, isErr, stream, inTok, outTok, cacheR, cacheC, costUSD, durMs, stopReason, toolsCnt, toolCalls, thinking, temp, ua, strings.ReplaceAll(errD, "\"", "'"), reqID)
		}
	}))

	// GET /api/glm/quota — Z.AI GLM usage quota (only if zai provider configured)
	mux.HandleFunc("GET /api/glm/quota", authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		apiKey := ""
		configPaths := []string{
			filepath.Join(appHome, "data", "config.json"),
			filepath.Join(workspace, "scripts", appName, "config.json"),
		}
		for _, p := range configPaths {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var raw struct {
				Providers map[string]struct {
					APIKey  string `json:"apiKey"`
					BaseURL string `json:"baseUrl"`
				} `json:"providers"`
			}
			if json.Unmarshal(data, &raw) == nil {
				if zai, ok := raw.Providers["zai"]; ok && zai.APIKey != "" {
					apiKey = zai.APIKey
				}
			}
			break
		}
		if apiKey == "" {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), "GET", "https://api.z.ai/api/monitor/usage/quota/limit", nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Accept", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Z.AI request failed: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))

	// GET /api/minimax/quota — MiniMax coding plan remains (only if minimax provider configured)
	mux.HandleFunc("GET /api/minimax/quota", authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		apiKey := ""
		configPaths := []string{
			filepath.Join(appHome, "data", "config.json"),
			filepath.Join(workspace, "scripts", appName, "config.json"),
		}
		for _, p := range configPaths {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var raw struct {
				Providers map[string]struct {
					APIKey  string `json:"apiKey"`
					BaseURL string `json:"baseUrl"`
				} `json:"providers"`
			}
			if json.Unmarshal(data, &raw) == nil {
				if mm, ok := raw.Providers["minimax"]; ok && mm.APIKey != "" {
					apiKey = mm.APIKey
				}
			}
			break
		}
		if apiKey == "" {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), "GET", "https://www.minimaxi.com/v1/api/openplatform/coding_plan/remains", nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "MiniMax request failed: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
}

// intParam parses an integer query parameter with a default.
func intParam(q url.Values, key string, def int) int {
	if v := q.Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ── CLI proxy detection ──

// proxyEnv returns the ANTHROPIC_BASE_URL env var pointing to the local proxy.
// In server mode, uses activeProxyPort directly.
// In CLI mode, detects if weiran server proxy is running.
var activeProxyPort int // set when proxy starts in server process

// serverOpenAIProxies caches per-provider OpenAI proxy ports started in server process.
// Key: provider name (string), Value: port (int). Used by /api/proxy/openai endpoint.
var serverOpenAIProxies sync.Map

var (
	detectedProxyPort int
	detectProxyOnce   sync.Once
	cachedOAuthToken  string // CLAUDE_CODE_OAUTH_TOKEN captured at startup
	oauthTokenOnce    sync.Once
)

// oauthToken returns the cached CLAUDE_CODE_OAUTH_TOKEN.
// Priority: env var > workspace/.oauth-token file > config.json server.oauthToken
func oauthToken() string {
	oauthTokenOnce.Do(func() {
		// 1. Environment variable
		if tok := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); tok != "" {
			cachedOAuthToken = tok
			fmt.Fprintf(os.Stderr, "[%s] CLAUDE_CODE_OAUTH_TOKEN from env\n", appName)
			return
		}
		// 2. File: workspace/.oauth-token (like .jira-token pattern)
		tokenFile := workspace + "/.oauth-token"
		if data, err := os.ReadFile(tokenFile); err == nil {
			if tok := strings.TrimSpace(string(data)); tok != "" {
				cachedOAuthToken = tok
				fmt.Fprintf(os.Stderr, "[%s] CLAUDE_CODE_OAUTH_TOKEN from %s\n", appName, tokenFile)
				return
			}
		}
		// 3. config.json server.oauthToken
		type oauthCfg struct {
			Server struct {
				OAuthToken string `json:"oauthToken"`
			} `json:"server"`
		}
		if data, err := os.ReadFile(appDir + "/config.json"); err == nil {
			var c oauthCfg
			if json.Unmarshal(data, &c) == nil && c.Server.OAuthToken != "" {
				cachedOAuthToken = c.Server.OAuthToken
				fmt.Fprintf(os.Stderr, "[%s] CLAUDE_CODE_OAUTH_TOKEN from config.json\n", appName)
			}
		}
	})
	return cachedOAuthToken
}

func proxyEnv() string {
	if activeProxyPort > 0 {
		return fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", activeProxyPort)
	}
	// CLI mode: try to detect running server proxy
	detectProxyOnce.Do(func() {
		detectedProxyPort = detectRemoteProxy()
	})
	if detectedProxyPort > 0 {
		return fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", detectedProxyPort)
	}
	return ""
}

// detectRemoteProxy checks if weiran server is running with proxy enabled.
// Reads config.json for proxy port, then does a quick HTTP probe.
func detectRemoteProxy() int {
	cfg := loadServerConfig()
	if !cfg.Proxy.Enabled {
		return 0
	}
	port := cfg.Proxy.Port
	if port == 0 {
		port = 9091
	}

	// Quick probe: connect to proxy port (it returns Anthropic API banner)
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return 0
	}
	resp.Body.Close()
	// Any response (200/404/etc.) means the proxy is listening
	fmt.Fprintf(os.Stderr, "[%s] detected server proxy on port %d\n", appName, port)
	return port
}

// injectProxyEnv appends the proxy env var to an env slice if the proxy is active.
// Uses the global overrideModel for provider detection (CLI mode).
func injectProxyEnv(env []string) []string {
	return injectProxyEnvWithModel(env, "", overrideModel)
}

// injectProxyEnvWithSession appends the proxy env var with an optional session ID path prefix.
// Uses the global overrideModel for provider detection (backward compat).
func injectProxyEnvWithSession(env []string, sessionID string) []string {
	return injectProxyEnvWithModel(env, sessionID, overrideModel)
}

// injectProxyEnvWithModel is the core env injection function.
// model: if provider/model format, routes directly to provider (skip local proxy).
// sessionID: if set, appends /s/{sessionID} to proxy URL for cost tracking.
func injectProxyEnvWithModel(env []string, sessionID string, model string) []string {
	// If model is a provider model, use provider endpoint directly (skip local proxy)
	overrideEnv, providerApplied := injectProviderEnv(env, model)
	if providerApplied {
		env = overrideEnv
	} else if e := proxyEnv(); e != "" {
		// If sessionID provided, append path prefix
		if sessionID != "" {
			// e is "ANTHROPIC_BASE_URL=http://127.0.0.1:9091"
			e = e + "/s/" + sessionID
		}
		// Remove any existing ANTHROPIC_BASE_URL to avoid conflict
		filtered := make([]string, 0, len(env)+2)
		for _, v := range env {
			if !strings.HasPrefix(v, "ANTHROPIC_BASE_URL=") {
				filtered = append(filtered, v)
			}
		}
		env = append(filtered, e)
	}

	// When a third-party provider (MiniMax, Z.AI, etc.) is active:
	// 1. Strip CLAUDE_CODE_OAUTH_TOKEN — otherwise Claude Code's login check prefers
	//    the OAuth token, then sends it to the non-Anthropic endpoint and gets 401.
	// 2. Inject CLAUDE_CODE_ENTRYPOINT — skips the interactive login check.
	//    Default is sdk-cli (official value). BUT providers that use ANTHROPIC_API_KEY
	//    (MiniMax — its Anthropic endpoint requires x-api-key header) hit Claude Code's
	//    `hasExternalApiKey` branch, which in v2.1.101+ still runs verifyApiKey even
	//    with sdk-cli and fails against the non-Anthropic endpoint ("Not logged in ·
	//    Please run /login"). For those, use sdk-go — an unrecognized entrypoint that
	//    slips past the verify path. Providers on ANTHROPIC_AUTH_TOKEN (GLM, Z.AI)
	//    take a different branch and work fine with sdk-cli.
	if providerApplied {
		usesApiKey := false
		for _, v := range env {
			if strings.HasPrefix(v, "ANTHROPIC_API_KEY=") {
				usesApiKey = true
				break
			}
		}
		entrypoint := "sdk-cli"
		if usesApiKey {
			entrypoint = "sdk-go"
		}
		filtered := make([]string, 0, len(env)+1)
		for _, v := range env {
			if strings.HasPrefix(v, "CLAUDE_CODE_OAUTH_TOKEN=") ||
				strings.HasPrefix(v, "CLAUDE_CODE_ENTRYPOINT=") {
				continue
			}
			filtered = append(filtered, v)
		}
		filtered = append(filtered, "CLAUDE_CODE_ENTRYPOINT="+entrypoint)
		return filtered
	}

	// Inject CLAUDE_CODE_OAUTH_TOKEN if available — ensures all spawned claude
	// processes share one static token instead of each doing OAuth refresh
	// (which causes race conditions with concurrent sessions).
	if tok := oauthToken(); tok != "" {
		filtered := make([]string, 0, len(env)+1)
		for _, v := range env {
			if !strings.HasPrefix(v, "CLAUDE_CODE_OAUTH_TOKEN=") {
				filtered = append(filtered, v)
			}
		}
		env = append(filtered, "CLAUDE_CODE_OAUTH_TOKEN="+tok)
	}

	// Inject CLAUDE_CONFIG_DIR only when the user explicitly set it (instance isolation).
	// Do NOT inject the default ~/.claude — explicitly setting it changes Claude Code's
	// behavior (triggers onboarding wizard, different auth flow).
	if claudeConfigDirExplicit {
		filtered := make([]string, 0, len(env)+1)
		for _, v := range env {
			if !strings.HasPrefix(v, "CLAUDE_CONFIG_DIR=") {
				filtered = append(filtered, v)
			}
		}
		env = append(filtered, "CLAUDE_CONFIG_DIR="+claudeConfigDir)
	}

	return env
}
