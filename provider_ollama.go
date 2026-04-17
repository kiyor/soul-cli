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

type ollamaAnthropicResponse struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"`
	Role         string                 `json:"role"`
	Content      []ollamaAnthropicBlock `json:"content"`
	Model        string                 `json:"model"`
	StopReason   string                 `json:"stop_reason"`
	StopSequence *string                `json:"stop_sequence"`
	Usage        ollamaAnthropicUsage   `json:"usage"`
}

type ollamaChatResponse struct {
	Choices []struct {
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []ollamaToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
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

func ollamaToolUseInput(arguments string) json.RawMessage {
	if arguments == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(arguments)) {
		return json.RawMessage(arguments)
	}
	b, _ := json.Marshal(map[string]string{"raw": arguments})
	return json.RawMessage(b)
}

// ollamaValidatedToolArgs returns validated JSON for a tool_call's accumulated
// arguments buffer. Small Ollama models often emit partial/malformed JSON; we
// fall back to {"raw": "..."} so the Anthropic client can always parse
// input_json_delta. Empty string returns "{}".
func ollamaValidatedToolArgs(arguments string) string {
	if arguments == "" {
		return "{}"
	}
	if json.Valid([]byte(arguments)) {
		return arguments
	}
	b, _ := json.Marshal(map[string]string{"raw": arguments})
	return string(b)
}

// mapOllamaFinishReason translates Ollama/OpenAI finish_reason to Anthropic
// stop_reason. Previously only "stop" and "tool_calls" were handled; "length"
// (truncation) and "content_filter" fell through to "end_turn", making CC
// believe the response completed normally.
//
//	tool_calls     → tool_use
//	length         → max_tokens
//	content_filter → stop_sequence (closest semantic; Anthropic has no exact)
//	stop/""        → tool_use if hasToolUse else end_turn
func mapOllamaFinishReason(fr string, hasToolUse bool) string {
	switch fr {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "stop_sequence"
	}
	if hasToolUse {
		return "tool_use"
	}
	return "end_turn"
}

func ollamaTranslateChoiceToAnthropic(model string, choice struct {
	Message struct {
		Role      string           `json:"role"`
		Content   string           `json:"content"`
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}, usage ollamaAnthropicUsage) ollamaAnthropicResponse {
	content := make([]ollamaAnthropicBlock, 0, 1+len(choice.Message.ToolCalls))
	if choice.Message.Content != "" {
		content = append(content, ollamaAnthropicBlock{
			Type: "text",
			Text: choice.Message.Content,
		})
	}
	for _, tc := range choice.Message.ToolCalls {
		toolUseID := "toolu_" + tc.ID
		if tc.ID == "" {
			toolUseID = "toolu_" + ollamaRandomID()
		}
		content = append(content, ollamaAnthropicBlock{
			Type:  "tool_use",
			ID:    toolUseID,
			Name:  tc.Function.Name,
			Input: ollamaToolUseInput(tc.Function.Arguments),
		})
	}

	stopReason := mapOllamaFinishReason(choice.FinishReason, len(choice.Message.ToolCalls) > 0)

	return ollamaAnthropicResponse{
		ID:           "msg_" + ollamaRandomID(),
		Type:         "message",
		Role:         "assistant",
		Content:      content,
		Model:        model,
		StopReason:   stopReason,
		StopSequence: nil,
		Usage:        usage,
	}
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
							co, ok := ci.(map[string]any)
							if !ok {
								continue
							}
							typ, _ := co["type"].(string)
							switch typ {
							case "text":
								if t, _ := co["text"].(string); t != "" {
									outputText += t
								}
							case "image":
								// Ollama OpenAI-compatible endpoint does not accept image
								// blocks in tool role messages. Emit a text placeholder so
								// the model at least knows an image was returned.
								outputText += "[image omitted: ollama provider does not support multimodal tool results]\n"
							default:
								// Unknown block type — try text fallback for forward compat.
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
		Stream:    areq.Stream,
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
	if areq.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

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

	if !areq.Stream {
		var upstream ollamaChatResponse
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "ollama upstream read: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := json.Unmarshal(body, &upstream); err != nil {
			http.Error(w, "ollama upstream decode: "+err.Error(), http.StatusBadGateway)
			return
		}

		choice := struct {
			Message struct {
				Role      string           `json:"role"`
				Content   string           `json:"content"`
				ToolCalls []ollamaToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		}{}
		if len(upstream.Choices) > 0 {
			choice = upstream.Choices[0]
		}

		writeJSON(w, http.StatusOK, ollamaTranslateChoiceToAnthropic(areq.Model, choice, ollamaAnthropicUsage{
			InputTokens:  upstream.Usage.PromptTokens,
			OutputTokens: upstream.Usage.CompletionTokens,
		}))
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
	finishReason := ""
	var outputTokens int

	// Track tool_use blocks: map from OpenAI tool_call index → Anthropic block index
	toolBlockMap := make(map[int]int) // ollama tc index → our block index
	toolBlockStarted := make(map[int]bool)
	// Accumulate tool_call arguments across streaming chunks. We buffer instead
	// of forwarding partial_json live because small Ollama models routinely
	// produce malformed JSON mid-stream; emitting a single validated delta at
	// block close keeps the Anthropic client's JSON parser happy.
	toolBlockArgs := make(map[int]string)
	toolBlockOrder := make([]int, 0) // preserve emit order: block indices in arrival order

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

		// Finish reason — captured here, translated to stop_reason at stream end
		// so all possible values (stop / tool_calls / length / content_filter)
		// get mapped via mapOllamaFinishReason instead of silently dropping to
		// "end_turn".
		if fr, _ := choice["finish_reason"].(string); fr != "" && fr != "null" {
			finishReason = fr
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
					toolBlockOrder = append(toolBlockOrder, blockIndex)
					blockIndex++
				}

				// Accumulate arguments; defer emit until block_stop so we can
				// validate the final JSON and fall back to {"raw":...} if the
				// upstream model produced malformed output.
				if funcArgs != "" {
					toolBlockArgs[toolBlockMap[tcIdx]] += funcArgs
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

	// Flush accumulated tool_use arguments with JSON validation, in arrival order.
	for _, bIdx := range toolBlockOrder {
		args := ollamaValidatedToolArgs(toolBlockArgs[bIdx])
		ollamaWriteSSE(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": bIdx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": args,
			},
		})
		ollamaWriteSSE(w, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": bIdx,
		})
	}

	hasToolUse := len(toolBlockOrder) > 0
	stopReason := mapOllamaFinishReason(finishReason, hasToolUse)

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
