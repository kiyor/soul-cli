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
	"bytes"
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read refresh response: %w", err)
	}
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
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`    // for tool_use blocks
	Name  string          `json:"name,omitempty"`  // for tool_use blocks
	Input json.RawMessage `json:"input,omitempty"` // for tool_use blocks
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
	Type  string         `json:"type"`
	Index int            `json:"index"`
	Delta map[string]any `json:"delta"`
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
	Model             string            `json:"model"`
	Store             bool              `json:"store"`
	Stream            bool              `json:"stream"`
	Instructions      string            `json:"instructions"`
	Input             []any             `json:"input"`
	Text              map[string]any    `json:"text"`
	Tools             []codexToolSpec   `json:"tools,omitempty"`
	ToolChoice        string            `json:"tool_choice"`
	ParallelToolCalls bool              `json:"parallel_tool_calls"`
}

// codexToolSpec is a Codex function tool definition (OpenAI Responses API format).
type codexToolSpec struct {
	Type        string         `json:"type"` // "function"
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict"`
}

// codexInputMessage is a Codex input message item.
type codexInputMessage struct {
	Type    string           `json:"type"` // "message"
	Role    string           `json:"role"`
	Content []codexInputItem `json:"content"`
}

// codexInputItem is a content item inside a Codex input message.
type codexInputItem struct {
	Type string `json:"type"` // "input_text" or "output_text"
	Text string `json:"text"`
}

// codexFunctionCall is a Codex function_call input item (tool invocation in history).
type codexFunctionCall struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// codexFunctionCallOutput is a Codex function_call_output input item (tool result in history).
type codexFunctionCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
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

// codexTranslateTools converts Anthropic tool definitions to Codex function tool specs.
func codexTranslateTools(tools []any) []codexToolSpec {
	var out []codexToolSpec
	for _, t := range tools {
		obj, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := obj["name"].(string)
		desc, _ := obj["description"].(string)
		params, _ := obj["input_schema"].(map[string]any)
		if name == "" {
			continue
		}
		out = append(out, codexToolSpec{
			Type:        "function",
			Name:        name,
			Description: desc,
			Parameters:  params,
			Strict:      false,
		})
	}
	return out
}

// codexTranslateMessages converts Anthropic messages (with tool_use/tool_result blocks)
// to Codex input items for the Responses API.
func codexTranslateMessages(messages []codexAnthropicMessage) []any {
	var out []any
	// Map tool_use_id (Anthropic) → call_id (Codex) for accurate reverse mapping.
	// Avoids TrimPrefix assumption that all IDs were generated by us.
	toolUseToCallID := make(map[string]string)
	for _, m := range messages {
		role := m.Role
		switch content := m.Content.(type) {
		case string:
			itemType := "input_text"
			if role == "assistant" {
				itemType = "output_text"
			}
			out = append(out, codexInputMessage{
				Type: "message",
				Role: role,
				Content: []codexInputItem{{Type: itemType, Text: content}},
			})
		case []any:
			// Collect text blocks into a message, emit tool blocks as separate items
			var textParts []codexInputItem
			itemType := "input_text"
			if role == "assistant" {
				itemType = "output_text"
			}
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := block["type"].(string)
				switch typ {
				case "text":
					if t, _ := block["text"].(string); t != "" {
						textParts = append(textParts, codexInputItem{Type: itemType, Text: t})
					}
				case "tool_use":
					// Flush pending text first
					if len(textParts) > 0 {
						out = append(out, codexInputMessage{
							Type: "message", Role: role, Content: textParts,
						})
						textParts = nil
					}
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					inputVal := block["input"]
					argBytes, _ := json.Marshal(inputVal)
					// Use the raw Anthropic ID as the Codex CallID, and record
					// the mapping so tool_result can look it up accurately.
					toolUseToCallID[id] = id
					out = append(out, codexFunctionCall{
						Type:      "function_call",
						CallID:    id,
						Name:      name,
						Arguments: string(argBytes),
					})
				case "tool_result":
					// Flush pending text first
					if len(textParts) > 0 {
						out = append(out, codexInputMessage{
							Type: "message", Role: role, Content: textParts,
						})
						textParts = nil
					}
					toolUseID, _ := block["tool_use_id"].(string)
					// Look up the original call_id from our mapping.
					// Falls back to TrimPrefix for IDs generated during
					// Codex→Anthropic streaming (where we prefixed "toolu_").
					callID, ok := toolUseToCallID[toolUseID]
					if !ok {
						callID = strings.TrimPrefix(toolUseID, "toolu_")
					}
					outputText := ""
					switch c := block["content"].(type) {
					case string:
						outputText = c
					case []any:
						for _, ci := range c {
							if co, ok := ci.(map[string]any); ok {
								if t, _ := co["text"].(string); t != "" {
									outputText += t
								}
							}
						}
					}
					out = append(out, codexFunctionCallOutput{
						Type:   "function_call_output",
						CallID: callID,
						Output: outputText,
					})
				}
			}
			// Flush remaining text
			if len(textParts) > 0 {
				out = append(out, codexInputMessage{
					Type: "message", Role: role, Content: textParts,
				})
			}
		}
	}
	return out
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
// with full tool use support (function_call ↔ tool_use).
func handleCodexMessages(w http.ResponseWriter, r *http.Request, sess *codexSession, chatURL string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Parse as generic map first to extract tools (not in typed struct)
	var rawReq map[string]any
	if err := json.Unmarshal(body, &rawReq); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
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

	// Translate Anthropic tools → Codex function tools
	var codexTools []codexToolSpec
	if toolsRaw, ok := rawReq["tools"].([]any); ok && len(toolsRaw) > 0 {
		codexTools = codexTranslateTools(toolsRaw)
	}

	// Translate messages with tool_use/tool_result support
	inputItems := codexTranslateMessages(areq.Messages)

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
		Input:             inputItems,
		Text:              map[string]any{"verbosity": "medium"},
		Tools:             codexTools,
		ToolChoice:        "auto",
		ParallelToolCalls: true,
	}

	reqBody, err := json.Marshal(creq)
	if err != nil {
		http.Error(w, "marshal request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	req, err := http.NewRequest(http.MethodPost, chatURL, bytes.NewReader(reqBody))
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

	// Stream Codex SSE → Anthropic SSE with tool support
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
	codexWriteSSE(w, "ping", map[string]string{"type": "ping"})
	if flusher != nil {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	stopReason := "end_turn"
	var outputTokens int
	blockIndex := 0
	textBlockStarted := false
	hasToolUse := false

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
				// Lazy-start a text block
				if !textBlockStarted {
					codexWriteSSE(w, "content_block_start", codexBlockStart{
						Type:         "content_block_start",
						Index:        blockIndex,
						ContentBlock: codexAnthropicBlock{Type: "text", Text: ""},
					})
					textBlockStarted = true
				}
				codexWriteSSE(w, "content_block_delta", codexBlockDelta{
					Type:  "content_block_delta",
					Index: blockIndex,
					Delta: map[string]any{"type": "text_delta", "text": delta},
				})
				if flusher != nil {
					flusher.Flush()
				}
			}

		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			if item == nil {
				continue
			}
			itemType, _ := item["type"].(string)

			if itemType == "function_call" {
				// Close any open text block first
				if textBlockStarted {
					codexWriteSSE(w, "content_block_stop", codexBlockStop{
						Type: "content_block_stop", Index: blockIndex,
					})
					blockIndex++
					textBlockStarted = false
				}

				hasToolUse = true
				callID, _ := item["call_id"].(string)
				name, _ := item["name"].(string)
				arguments, _ := item["arguments"].(string)

				// Generate Anthropic tool_use_id from call_id
				toolUseID := "toolu_" + callID
				if callID == "" {
					toolUseID = "toolu_" + codexRandomID()
				}

				// Emit content_block_start for tool_use
				codexWriteSSE(w, "content_block_start", codexBlockStart{
					Type:  "content_block_start",
					Index: blockIndex,
					ContentBlock: codexAnthropicBlock{
						Type:  "tool_use",
						ID:    toolUseID,
						Name:  name,
						Input: json.RawMessage("{}"),
					},
				})

				// Emit the full arguments as input_json_delta
				if arguments != "" {
					codexWriteSSE(w, "content_block_delta", codexBlockDelta{
						Type:  "content_block_delta",
						Index: blockIndex,
						Delta: map[string]any{
							"type":         "input_json_delta",
							"partial_json": arguments,
						},
					})
				}

				// Close tool_use block
				codexWriteSSE(w, "content_block_stop", codexBlockStop{
					Type: "content_block_stop", Index: blockIndex,
				})
				blockIndex++

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

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] codex SSE scanner error: %v\n", appName, err)
	}

	// Close any remaining open text block
	if textBlockStarted {
		codexWriteSSE(w, "content_block_stop", codexBlockStop{
			Type: "content_block_stop", Index: blockIndex,
		})
	}

	// Set stop reason based on whether tools were called
	if hasToolUse {
		stopReason = "tool_use"
	}

	msgDelta := codexMsgDelta{Type: "message_delta", Usage: codexAnthropicUsage{OutputTokens: outputTokens}}
	msgDelta.Delta.StopReason = stopReason
	codexWriteSSE(w, "message_delta", msgDelta)
	codexWriteSSE(w, "message_stop", codexMsgStop{Type: "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}
