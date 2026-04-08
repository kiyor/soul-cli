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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	atomic.AddInt64(&acc.InputTokens, input)
	atomic.AddInt64(&acc.OutputTokens, output)
	atomic.AddInt64(&acc.CacheReadTokens, cacheRead)
	atomic.AddInt64(&acc.CacheCreationTokens, cacheCreate)
	atomic.AddInt64(&acc.RequestCount, 1)

	// Total
	atomic.AddInt64(&ps.Total.InputTokens, input)
	atomic.AddInt64(&ps.Total.OutputTokens, output)
	atomic.AddInt64(&ps.Total.CacheReadTokens, cacheRead)
	atomic.AddInt64(&ps.Total.CacheCreationTokens, cacheCreate)
	atomic.AddInt64(&ps.Total.RequestCount, 1)
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
	proxyDB.Exec("PRAGMA journal_mode=WAL")
	proxyDB.Exec("PRAGMA busy_timeout=5000")

	_, err = proxyDB.Exec(`CREATE TABLE IF NOT EXISTS proxy_requests (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		time          TEXT NOT NULL,
		model         TEXT NOT NULL DEFAULT '',
		status        INTEGER NOT NULL DEFAULT 0,
		is_error      INTEGER NOT NULL DEFAULT 0,
		stream        INTEGER NOT NULL DEFAULT 0,
		input_tokens  INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read    INTEGER NOT NULL DEFAULT 0,
		cache_create  INTEGER NOT NULL DEFAULT 0,
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

	// Index for common queries
	proxyDB.Exec("CREATE INDEX IF NOT EXISTS idx_proxy_time ON proxy_requests(time)")
	proxyDB.Exec("CREATE INDEX IF NOT EXISTS idx_proxy_model ON proxy_requests(model)")

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
		_, err := proxyDB.Exec(`INSERT INTO proxy_requests
			(time, model, status, is_error, stream, input_tokens, output_tokens,
			 cache_read, cache_create, duration_ms, stop_reason, tools_count,
			 tool_calls, thinking_mode, temperature, user_agent, error_detail, request_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			info.StartTime.Format(time.RFC3339),
			info.Model, info.Status, isErr, isStream,
			info.InputTokens, info.OutputTokens, info.CacheRead, info.CacheCreate,
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

			// Strip our internal headers before sending upstream
			// (they start with X-Proxy- but we keep them on req for ModifyResponse to read)
		},
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

		query := fmt.Sprintf(
			"SELECT id,time,model,status,is_error,stream,input_tokens,output_tokens,cache_read,cache_create,duration_ms,stop_reason,tools_count,tool_calls,thinking_mode,temperature,user_agent,error_detail,request_id FROM proxy_requests WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?",
			strings.Join(where, " AND "),
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
				ts, model, stopReason, toolCalls, thinking, temp, ua, errD, reqID         string
			)
			rows.Scan(&id, &ts, &model, &status, &isErr, &stream, &inTok, &outTok, &cacheR, &cacheC, &durMs, &stopReason, &toolsCnt, &toolCalls, &thinking, &temp, &ua, &errD, &reqID)
			records = append(records, map[string]any{
				"id": id, "time": ts, "model": model, "status": status,
				"is_error": isErr == 1, "stream": stream == 1,
				"input_tokens": inTok, "output_tokens": outTok,
				"cache_read": cacheR, "cache_create": cacheC,
				"duration_ms": durMs, "stop_reason": stopReason,
				"tools_count": toolsCnt, "tool_calls": toolCalls,
				"thinking_mode": thinking, "temperature": temp,
				"user_agent": ua, "error_detail": errD, "request_id": reqID,
			})
		}
		if records == nil {
			records = []map[string]any{}
		}

		// Get total count
		countQuery := fmt.Sprintf("SELECT COUNT(*) FROM proxy_requests WHERE %s", strings.Join(where, " AND "))
		countArgs := args[:len(args)-2] // strip limit and offset
		var total int
		proxyDB.QueryRow(countQuery, countArgs...).Scan(&total)

		writeJSON(w, http.StatusOK, map[string]any{
			"records": records,
			"total":   total,
			"limit":   limit,
			"offset":  offset,
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
				avgDur                                               float64
			)
			rows.Scan(&period, &model, &requests, &inTok, &outTok, &cacheR, &cacheC, &errors, &avgDur)
			stats = append(stats, map[string]any{
				"period": period, "model": model, "requests": requests,
				"input_tokens": inTok, "output_tokens": outTok,
				"cache_read": cacheR, "cache_create": cacheC,
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

var (
	detectedProxyPort int
	detectProxyOnce   sync.Once
)

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
func injectProxyEnv(env []string) []string {
	if e := proxyEnv(); e != "" {
		// Remove any existing ANTHROPIC_BASE_URL to avoid conflict
		filtered := make([]string, 0, len(env)+1)
		for _, v := range env {
			if !strings.HasPrefix(v, "ANTHROPIC_BASE_URL=") {
				filtered = append(filtered, v)
			}
		}
		return append(filtered, e)
	}
	return env
}
