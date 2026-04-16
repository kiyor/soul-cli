package main

// provider_ollama.go — embedded Anthropic→Ollama protocol proxy
//
// When a provider has Type=="ollama", injectProviderEnv starts a local HTTP server
// that translates Anthropic /v1/messages requests to Ollama /v1/chat/completions
// (standard OpenAI-compatible format).
// Claude Code talks Anthropic protocol; the proxy silently translates to Ollama.

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
	"os"
	"strings"
	"sync"
	"time"
)

// ── Ollama (OpenAI-compatible) request types ──────────────────────────────────

type ollamaChatRequest struct {
	Model     string              `json:"model"`
	Messages  []ollamaChatMessage `json:"messages"`
	Tools     []ollamaToolSpec    `json:"tools,omitempty"`
	Stream    bool                `json:"stream"`
	MaxTokens int                 `json:"max_tokens,omitempty"`
}

type ollamaChatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type ollamaToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function ollamaFunctionCall `json:"function"`
}

type ollamaFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ollamaToolSpec struct {
	Type     string        `json:"type"`
	Function ollamaFuncDef `json:"function"`
}

type ollamaFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ── Anthropic SSE response types ──────────────────────────────────────────────

type ollamaBlockStart struct {
	Type         string               `json:"type"`
	Index        int                  `json:"index"`
	ContentBlock ollamaAnthropicBlock `json:"content_block"`
}

type ollamaAnthropicBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type ollamaAnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func ollamaRandomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func ollamaWriteSSE(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(b))
}

// ollamaExtractSystemText extracts system prompt from Anthropic format.
func ollamaExtractSystemText(system any) string {
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

// ollamaTranslateTools converts Anthropic tool definitions to OpenAI function tools.
func ollamaTranslateTools(tools []any) []ollamaToolSpec {
	var out []ollamaToolSpec
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
		out = append(out, ollamaToolSpec{
			Type: "function",
			Function: ollamaFuncDef{
				Name:        name,
				Description: desc,
				Parameters:  params,
			},
		})
	}
	return out
}

// ollamaAnthropicMsg is a message in Anthropic format (role + flexible content).
type ollamaAnthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// ollamaTranslateMessages converts Anthropic messages (with tool_use/tool_result)
// to OpenAI-compatible chat messages.
func ollamaTranslateMessages(messages []ollamaAnthropicMsg) []ollamaChatMessage {
	var out []ollamaChatMessage
	for _, m := range messages {
		switch content := m.Content.(type) {
		case string:
			out = append(out, ollamaChatMessage{Role: m.Role, Content: content})

		case []any:
			var textParts []string
			var toolCalls []ollamaToolCall
			var toolResults []ollamaChatMessage

			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := block["type"].(string)
				switch typ {
				case "text":
					if t, _ := block["text"].(string); t != "" {
						textParts = append(textParts, t)
					}
				case "tool_use":
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					inputVal := block["input"]
					argBytes, _ := json.Marshal(inputVal)
					toolCalls = append(toolCalls, ollamaToolCall{
						ID:   id,
						Type: "function",
						Function: ollamaFunctionCall{
							Name:      name,
							Arguments: string(argBytes),
						},
					})
				case "tool_result":
					toolUseID, _ := block["tool_use_id"].(string)
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
					toolResults = append(toolResults, ollamaChatMessage{
						Role:       "tool",
						Content:    outputText,
						ToolCallID: toolUseID,
					})
				}
			}

			if len(toolCalls) > 0 {
				text := strings.Join(textParts, "\n")
				if text == "" {
					text = " "
				}
				out = append(out, ollamaChatMessage{
					Role:      m.Role,
					Content:   text,
					ToolCalls: toolCalls,
				})
			} else if len(textParts) > 0 {
				out = append(out, ollamaChatMessage{
					Role:    m.Role,
					Content: strings.Join(textParts, "\n"),
				})
			}

			out = append(out, toolResults...)
		}
	}
	return out
}

// ── Server proxy delegation ───────────────────────────────────────────────────

var serverOllamaProxies sync.Map

func detectServerOllamaProxy(providerName string) int {
	cfg := loadServerConfig()
	if cfg.Token == "" {
		return 0
	}
	serverURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(serverURL + "/api/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0
	}
	resp.Body.Close()

	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/proxy/ollama?provider=%s", serverURL, providerName), nil)
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
	fmt.Fprintf(os.Stderr, "[%s] reusing server ollama proxy on port %d\n", appName, result.Port)
	return result.Port
}

// ── Embedded proxy ────────────────────────────────────────────────────────────

var activeOllamaProxyPort int

func startOllamaProxy(provider providerConfig) (int, error) {
	baseURL := strings.TrimRight(provider.BaseURL, "")
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	chatURL := baseURL + "/v1/chat/completions"

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
		handleOllamaMessages(w, r, chatURL)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok","proxy":"weiran/ollama"}`)
	})

	go func() {
		if err := http.Serve(ln, mux); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] ollama proxy stopped: %v\n", appName, err)
		}
	}()

	activeOllamaProxyPort = port
	fmt.Fprintf(os.Stderr, "[%s] ollama proxy started on http://127.0.0.1:%d → %s\n", appName, port, chatURL)
	return port, nil
}

// ── Main translation handler ──────────────────────────────────────────────────

func handleOllamaMessages(w http.ResponseWriter, r *http.Request, chatURL string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Parse as raw map first (to extract tools), then as typed struct
	var rawReq map[string]any
	if err := json.Unmarshal(body, &rawReq); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	var areq struct {
		Model     string               `json:"model"`
		MaxTokens int                  `json:"max_tokens"`
		System    any                  `json:"system,omitempty"`
		Messages  []ollamaAnthropicMsg `json:"messages"`
		Stream    bool                 `json:"stream"`
	}
	if err := json.Unmarshal(body, &areq); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// System prompt
	systemText := ollamaExtractSystemText(areq.System)

	// Translate messages
	chatMsgs := ollamaTranslateMessages(areq.Messages)
	if systemText != "" {
		chatMsgs = append([]ollamaChatMessage{{Role: "system", Content: systemText}}, chatMsgs...)
	}

	// Translate tools
	var ollamaTools []ollamaToolSpec
	if toolsRaw, ok := rawReq["tools"].([]any); ok && len(toolsRaw) > 0 {
		ollamaTools = ollamaTranslateTools(toolsRaw)
	}

	// Model name: strip provider prefix
	modelName := areq.Model
	if idx := strings.Index(modelName, "/"); idx >= 0 {
		modelName = modelName[idx+1:]
	}

	maxTokens := areq.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	creq := ollamaChatRequest{
		Model:     modelName,
		Messages:  chatMsgs,
		Tools:     ollamaTools,
		Stream:    true,
		MaxTokens: maxTokens,
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "ollama upstream: "+err.Error(), http.StatusBadGateway)
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

	// ── Stream Ollama SSE → Anthropic SSE ──────────────────────────────────
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	msgID := "msg_" + ollamaRandomID()

	// message_start
	ollamaWriteSSE(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"content": []any{}, "model": areq.Model,
			"stop_reason": "", "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	ollamaWriteSSE(w, "ping", map[string]string{"type": "ping"})
	if flusher != nil {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	blockIndex := 0
	textBlockStarted := false
	stopReason := "end_turn"
	var outputTokens int

	// Track tool_use blocks: map from OpenAI tool_call index → Anthropic block index
	toolBlockMap := make(map[int]int) // ollama tc index → our block index
	toolBlockStarted := make(map[int]bool)

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

		// Usage from top-level
		if usageRaw, ok := event["usage"].(map[string]any); ok {
			if ot, ok := usageRaw["completion_tokens"].(float64); ok {
				outputTokens = int(ot)
			}
		}

		choices, _ := event["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}

		// Finish reason
		if fr, _ := choice["finish_reason"].(string); fr != "" && fr != "null" {
			if fr == "tool_calls" {
				stopReason = "tool_use"
			} else if fr == "stop" {
				stopReason = "end_turn"
			}
		}

		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}

		// ── Text content ──
		if content, _ := delta["content"].(string); content != "" {
			if !textBlockStarted {
				ollamaWriteSSE(w, "content_block_start", ollamaBlockStart{
					Type: "content_block_start", Index: blockIndex,
					ContentBlock: ollamaAnthropicBlock{Type: "text", Text: ""},
				})
				textBlockStarted = true
			}
			ollamaWriteSSE(w, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": content},
			})
			if flusher != nil {
				flusher.Flush()
			}
		}

		// ── Tool calls ──
		if tcRaw, ok := delta["tool_calls"].([]any); ok {
			for _, tcItem := range tcRaw {
				tcMap, _ := tcItem.(map[string]any)
				if tcMap == nil {
					continue
				}

				// Close text block first
				if textBlockStarted {
					ollamaWriteSSE(w, "content_block_stop", map[string]any{
						"type": "content_block_stop", "index": blockIndex,
					})
					blockIndex++
					textBlockStarted = false
				}

				tcID, _ := tcMap["id"].(string)
				tcIdx := 0
				if idx, ok := tcMap["index"].(float64); ok {
					tcIdx = int(idx)
				}

				funcMap, _ := tcMap["function"].(map[string]any)
				if funcMap == nil {
					continue
				}
				funcName, _ := funcMap["name"].(string)
				funcArgs, _ := funcMap["arguments"].(string)

				// First chunk for this tool call: emit block_start
				if !toolBlockStarted[tcIdx] {
					toolUseID := "toolu_" + tcID
					if tcID == "" {
						toolUseID = "toolu_" + ollamaRandomID()
					}

					ollamaWriteSSE(w, "content_block_start", ollamaBlockStart{
						Type: "content_block_start", Index: blockIndex,
						ContentBlock: ollamaAnthropicBlock{
							Type: "tool_use", ID: toolUseID,
							Name: funcName, Input: json.RawMessage("{}"),
						},
					})
					toolBlockMap[tcIdx] = blockIndex
					toolBlockStarted[tcIdx] = true
					blockIndex++
				}

				// Arguments delta
				if funcArgs != "" {
					bIdx := toolBlockMap[tcIdx]
					ollamaWriteSSE(w, "content_block_delta", map[string]any{
						"type": "content_block_delta", "index": bIdx,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": funcArgs,
						},
					})
				}

				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] ollama SSE scanner error: %v\n", appName, err)
	}

	// Close remaining text block
	if textBlockStarted {
		ollamaWriteSSE(w, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": blockIndex,
		})
		blockIndex++
	}

	// Close tool_use blocks
	for _, bIdx := range toolBlockMap {
		ollamaWriteSSE(w, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": bIdx,
		})
	}

	if len(toolBlockStarted) > 0 {
		stopReason = "tool_use"
	}

	// message_delta + message_stop
	ollamaWriteSSE(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": stopReason, "stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": outputTokens},
	})
	ollamaWriteSSE(w, "message_stop", map[string]string{"type": "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}
