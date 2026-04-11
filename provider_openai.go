package main

// provider_openai.go — embedded Anthropic→Codex protocol proxy
//
// When a provider has Type=="openai", injectProviderEnv starts a local HTTP server
// that translates Anthropic /v1/messages requests to Codex /codex/responses.
// Claude Code talks Anthropic protocol; the proxy silently translates to ChatGPT Plus (Codex).
//
// Auth source: ~/.codex/auth.json (written by `codex login`)

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	codexEndpoint = "https://chatgpt.com/backend-api/codex/responses"
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexTokenURL = "https://auth.openai.com/oauth/token"
	codexAuthFile = "~/.codex/auth.json"
)

// ── Auth ──────────────────────────────────────────────────────────────────────

type codexAuthJSON struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

type codexSession struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
}

func loadCodexAuth(authFile string) (*codexSession, error) {
	if authFile == "" {
		authFile = codexAuthFile
	}
	path := expandPath(authFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (run: codex login)", authFile, err)
	}
	var f codexAuthJSON
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode codex auth: %w", err)
	}
	if f.Tokens.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in %s", authFile)
	}
	return &codexSession{
		AccessToken:  f.Tokens.AccessToken,
		RefreshToken: f.Tokens.RefreshToken,
		AccountID:    f.Tokens.AccountID,
	}, nil
}

// expandPath expands ~ to the home directory.
func expandPath(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}

// decodeJWTExp extracts the exp claim from a JWT without signature validation.
func decodeJWTExp(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	pad := (4 - len(parts[1])%4) % 4
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1] + strings.Repeat("=", pad))
	if err != nil {
		return time.Time{}, false
	}
	var payload struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &payload); err != nil || payload.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(payload.Exp, 0), true
}

type codexTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func refreshCodexToken(sess *codexSession) error {
	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("client_id", codexClientID)
	values.Set("refresh_token", sess.RefreshToken)

	req, err := http.NewRequest(http.MethodPost, codexTokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("refresh failed %s: %s", resp.Status, body)
	}
	var tr codexTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("decode refresh: %w", err)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("no access_token in refresh response")
	}
	sess.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		sess.RefreshToken = tr.RefreshToken
	}
	return nil
}

func ensureCodexTokenFresh(sess *codexSession) error {
	exp, ok := decodeJWTExp(sess.AccessToken)
	if ok && time.Now().Before(exp.Add(-30*time.Second)) {
		return nil
	}
	fmt.Fprintf(os.Stderr, "[%s] codex token expired, refreshing...\n", appName)
	return refreshCodexToken(sess)
}

func codexUserAgent() string {
	return fmt.Sprintf("pi (%s; %s)", runtime.GOOS, runtime.GOARCH)
}

// ── Anthropic request/response types ─────────────────────────────────────────

type codexAnthropicRequest struct {
	Model     string                  `json:"model"`
	MaxTokens int                     `json:"max_tokens"`
	System    any                     `json:"system,omitempty"`
	Messages  []codexAnthropicMessage `json:"messages"`
	Stream    bool                    `json:"stream"`
}

type codexAnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type codexAnthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type codexAnthropicResponse struct {
	ID           string                `json:"id"`
	Type         string                `json:"type"`
	Role         string                `json:"role"`
	Content      []codexAnthropicBlock `json:"content"`
	Model        string                `json:"model"`
	StopReason   string                `json:"stop_reason"`
	StopSequence *string               `json:"stop_sequence"`
	Usage        codexAnthropicUsage   `json:"usage"`
}

type codexAnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// SSE event types
type codexMsgStart struct {
	Type    string                 `json:"type"`
	Message codexAnthropicResponse `json:"message"`
}

type codexBlockStart struct {
	Type         string               `json:"type"`
	Index        int                  `json:"index"`
	ContentBlock codexAnthropicBlock  `json:"content_block"`
}

type codexBlockDelta struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

type codexBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type codexMsgDelta struct {
	Type  string `json:"type"`
	Delta struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage codexAnthropicUsage `json:"usage"`
}

type codexMsgStop struct {
	Type string `json:"type"`
}

// ── Codex request ─────────────────────────────────────────────────────────────

type codexAPIRequest struct {
	Model             string         `json:"model"`
	Store             bool           `json:"store"`
	Stream            bool           `json:"stream"`
	Instructions      string         `json:"instructions"`
	Input             []codexMessage `json:"input"`
	Text              map[string]any `json:"text"`
	ToolChoice        string         `json:"tool_choice"`
	ParallelToolCalls bool           `json:"parallel_tool_calls"`
}

type codexMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ── Protocol translation helpers ──────────────────────────────────────────────

func codexExtractSystemText(system any) string {
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := obj["text"].(string); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func codexExtractContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := obj["type"].(string)
			if typ == "text" {
				if t, _ := obj["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func codexWriteSSE(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(b))
}

func codexRandomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ── Server proxy delegation ───────────────────────────────────────────────────

// detectServerOpenAIProxy checks if the weiran server is running and asks it to
// start (or reuse) an OpenAI proxy for the given provider. Returns the proxy port,
// or 0 if the server is not available or does not support the provider.
//
// When the server manages the proxy, CLI can use syscall.Exec normally (the proxy
// goroutine lives in the long-running server process, not the ephemeral CLI process).
func detectServerOpenAIProxy(providerName string) int {
	cfg := loadServerConfig()
	if cfg.Token == "" {
		return 0
	}

	serverURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)

	// Quick health check (200ms timeout)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(serverURL + "/api/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0
	}
	resp.Body.Close()

	// Ask server to start/get proxy for this provider
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/proxy/openai?provider=%s", serverURL, providerName), nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0
	}
	defer resp.Body.Close()

	var result struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.Port == 0 {
		return 0
	}

	fmt.Fprintf(os.Stderr, "[%s] reusing server openai proxy on port %d\n", appName, result.Port)
	return result.Port
}

// ── Embedded proxy ────────────────────────────────────────────────────────────

// activeOpenAIProxyPort is non-zero when an embedded OpenAI proxy is running.
// Used by execClaude() to detect that syscall.Exec cannot be used (exec replaces
// the process image, killing the proxy goroutine). CLI modes must use subprocess.
var activeOpenAIProxyPort int

// startOpenAIProxy starts a local HTTP server that translates Anthropic /v1/messages
// to Codex /codex/responses. Returns the port the server is listening on.
func startOpenAIProxy(provider providerConfig) (int, error) {
	authFile := provider.AuthFile
	sess, err := loadCodexAuth(authFile)
	if err != nil {
		return 0, fmt.Errorf("codex auth: %w", err)
	}

	chatURL := provider.ChatURL
	if chatURL == "" {
		chatURL = codexEndpoint
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleCodexMessages(w, r, sess, chatURL)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok","proxy":"weiran/codex"}`)
	})

	go func() {
		if err := http.Serve(ln, mux); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] codex proxy stopped: %v\n", appName, err)
		}
	}()

	activeOpenAIProxyPort = port
	fmt.Fprintf(os.Stderr, "[%s] codex proxy started on http://127.0.0.1:%d\n", appName, port)
	return port, nil
}

// handleCodexMessages translates Anthropic /v1/messages → Codex /codex/responses
func handleCodexMessages(w http.ResponseWriter, r *http.Request, sess *codexSession, chatURL string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var areq codexAnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := ensureCodexTokenFresh(sess); err != nil {
		http.Error(w, "token refresh: "+err.Error(), http.StatusUnauthorized)
		return
	}

	instructions := codexExtractSystemText(areq.System)
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}

	var inputMsgs []codexMessage
	for _, m := range areq.Messages {
		inputMsgs = append(inputMsgs, codexMessage{
			Role:    m.Role,
			Content: codexExtractContentText(m.Content),
		})
	}

	// use model from request if gpt-prefixed, else fall back to endpoint default
	targetModel := areq.Model
	if !strings.HasPrefix(targetModel, "gpt-") {
		targetModel = "gpt-4.1" // safe default for codex endpoint
	}

	creq := codexAPIRequest{
		Model:             targetModel,
		Store:             false,
		Stream:            true,
		Instructions:      instructions,
		Input:             inputMsgs,
		Text:              map[string]any{"verbosity": "medium"},
		ToolChoice:        "auto",
		ParallelToolCalls: true,
	}

	reqBody, _ := json.Marshal(creq)
	req, err := http.NewRequest(http.MethodPost, chatURL, strings.NewReader(string(reqBody)))
	if err != nil {
		http.Error(w, "build request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+sess.AccessToken)
	req.Header.Set("chatgpt-account-id", sess.AccountID)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "pi")
	req.Header.Set("User-Agent", codexUserAgent())
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		upBody, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		errMsg, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "api_error", "message": string(upBody)},
		})
		w.Write(errMsg)
		return
	}

	// Stream Codex SSE → Anthropic SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	msgID := "msg_" + codexRandomID()

	codexWriteSSE(w, "message_start", codexMsgStart{
		Type: "message_start",
		Message: codexAnthropicResponse{
			ID:      msgID,
			Type:    "message",
			Role:    "assistant",
			Content: []codexAnthropicBlock{},
			Model:   targetModel,
			Usage:   codexAnthropicUsage{},
		},
	})
	codexWriteSSE(w, "content_block_start", codexBlockStart{
		Type:         "content_block_start",
		Index:        0,
		ContentBlock: codexAnthropicBlock{Type: "text", Text: ""},
	})
	codexWriteSSE(w, "ping", map[string]string{"type": "ping"})
	if flusher != nil {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	stopReason := "end_turn"
	var outputTokens int

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "response.output_text.delta":
			delta, _ := event["delta"].(string)
			if delta != "" {
				bd := codexBlockDelta{Type: "content_block_delta", Index: 0}
				bd.Delta.Type = "text_delta"
				bd.Delta.Text = delta
				codexWriteSSE(w, "content_block_delta", bd)
				if flusher != nil {
					flusher.Flush()
				}
			}
		case "response.completed":
			if respObj, ok := event["response"].(map[string]any); ok {
				if usage, ok := respObj["usage"].(map[string]any); ok {
					if ot, ok := usage["output_tokens"].(float64); ok {
						outputTokens = int(ot)
					}
				}
			}
		}
	}

	codexWriteSSE(w, "content_block_stop", codexBlockStop{Type: "content_block_stop", Index: 0})
	msgDelta := codexMsgDelta{Type: "message_delta", Usage: codexAnthropicUsage{OutputTokens: outputTokens}}
	msgDelta.Delta.StopReason = stopReason
	codexWriteSSE(w, "message_delta", msgDelta)
	codexWriteSSE(w, "message_stop", codexMsgStop{Type: "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}
