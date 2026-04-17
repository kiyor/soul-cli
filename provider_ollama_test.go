package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveProvider_AllowsOllamaWithoutBaseURL(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"providers": {
			"ollama": {
				"type": "ollama",
				"models": ["llama3.2"]
			}
		}
	}`
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "config.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	origAppHome := appHome
	appHome = dir
	defer func() { appHome = origAppHome }()

	prov := resolveProvider("ollama")
	if prov == nil {
		t.Fatal("resolveProvider(ollama) = nil, want provider")
	}
	if prov.Type != "ollama" {
		t.Fatalf("resolveProvider(ollama) type = %q, want ollama", prov.Type)
	}
}

func TestHandleOllamaMessages_NonStreamReturnsAnthropicJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}

		var req ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if req.Stream {
			t.Fatal("upstream request stream = true, want false")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": "hello from ollama",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     12,
				"completion_tokens": 34,
			},
		})
	}))
	defer upstream.Close()

	body := `{
		"model":"ollama/llama3.2",
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"max_tokens":128
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handleOllamaMessages(rec, req, upstream.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var resp ollamaAnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode anthropic response: %v", err)
	}
	if resp.Type != "message" || resp.Role != "assistant" {
		t.Fatalf("unexpected response envelope: %+v", resp)
	}
	if resp.Model != "ollama/llama3.2" {
		t.Fatalf("model = %q, want ollama/llama3.2", resp.Model)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 34 {
		t.Fatalf("usage = %+v, want input=12 output=34", resp.Usage)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "hello from ollama" {
		t.Fatalf("content = %+v, want single text block", resp.Content)
	}
}

func TestMapOllamaFinishReason(t *testing.T) {
	tests := []struct {
		name       string
		finish     string
		hasToolUse bool
		want       string
	}{
		{"stop without tool → end_turn", "stop", false, "end_turn"},
		{"stop with tool → tool_use", "stop", true, "tool_use"},
		{"tool_calls → tool_use", "tool_calls", false, "tool_use"},
		{"length → max_tokens", "length", false, "max_tokens"},
		{"length even with tool → max_tokens", "length", true, "max_tokens"},
		{"content_filter → stop_sequence", "content_filter", false, "stop_sequence"},
		{"empty without tool → end_turn", "", false, "end_turn"},
		{"empty with tool → tool_use", "", true, "tool_use"},
		{"unknown reason without tool → end_turn", "novel_reason", false, "end_turn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapOllamaFinishReason(tc.finish, tc.hasToolUse)
			if got != tc.want {
				t.Fatalf("mapOllamaFinishReason(%q, %v) = %q, want %q",
					tc.finish, tc.hasToolUse, got, tc.want)
			}
		})
	}
}

func TestOllamaValidatedToolArgs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty → {}", "", "{}"},
		{"valid JSON passes through", `{"a":1}`, `{"a":1}`},
		{"valid JSON with whitespace", `{ "a" : 1 }`, `{ "a" : 1 }`},
		{"malformed JSON wrapped as raw", `{"a":`, `{"raw":"{\"a\":"}`},
		{"plain text wrapped as raw", `garbage`, `{"raw":"garbage"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaValidatedToolArgs(tc.in)
			if got != tc.want {
				t.Fatalf("ollamaValidatedToolArgs(%q) = %q, want %q",
					tc.in, got, tc.want)
			}
			// Invariant: result must always be valid JSON so the Anthropic
			// client's partial_json parser can consume it.
			if !json.Valid([]byte(got)) {
				t.Fatalf("result is not valid JSON: %q", got)
			}
		})
	}
}

func TestHandleOllamaMessages_FinishReasonLengthMapsMaxTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message":       map[string]any{"role": "assistant", "content": "truncated..."},
					"finish_reason": "length",
				},
			},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2},
		})
	}))
	defer upstream.Close()

	body := `{"model":"ollama/llama3.2","messages":[{"role":"user","content":"hi"}],"stream":false,"max_tokens":4}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleOllamaMessages(rec, req, upstream.URL)

	var resp ollamaAnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens (finish_reason=length)", resp.StopReason)
	}
}

func TestHandleOllamaMessages_ToolResultImageBlockReplacedWithPlaceholder(t *testing.T) {
	var captured ollamaChatRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer upstream.Close()

	body := `{
		"model":"ollama/llama3.2","stream":false,"max_tokens":64,
		"messages":[
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":[
				{"type":"text","text":"before"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
				{"type":"text","text":"after"}
			]}]}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	handleOllamaMessages(httptest.NewRecorder(), req, upstream.URL)

	// The tool message should contain both text parts and a placeholder noting
	// the image was dropped; the raw base64 image MUST NOT leak through (both
	// for token budget and for "we don't actually support multimodal" honesty).
	var toolMsg *ollamaChatMessage
	for i := range captured.Messages {
		if captured.Messages[i].Role == "tool" {
			toolMsg = &captured.Messages[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool message in upstream request: %+v", captured.Messages)
	}
	if !strings.Contains(toolMsg.Content, "before") || !strings.Contains(toolMsg.Content, "after") {
		t.Fatalf("tool content missing text parts: %q", toolMsg.Content)
	}
	if !strings.Contains(toolMsg.Content, "[image omitted") {
		t.Fatalf("tool content missing image placeholder: %q", toolMsg.Content)
	}
	if strings.Contains(toolMsg.Content, "AAAA") {
		t.Fatalf("raw base64 leaked into tool content: %q", toolMsg.Content)
	}
}

func TestHandleOllamaMessages_StreamMalformedToolArgsValidated(t *testing.T) {
	// Upstream emits two SSE chunks with a malformed JSON arguments stream
	// (missing close brace). Our proxy should accumulate and wrap as
	// {"raw":"..."} rather than forwarding broken partial_json.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// First chunk: tool call starts with partial JSON "{\"path\":"
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}` + "\n\n"))
		if fl != nil {
			fl.Flush()
		}
		// Second chunk: more partial garbage, never closed
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp/x"}}]}}]}` + "\n\n"))
		if fl != nil {
			fl.Flush()
		}
		// finish
		_, _ = w.Write([]byte(`data: {"choices":[{"finish_reason":"tool_calls","delta":{}}],"usage":{"completion_tokens":5}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	body := `{"model":"ollama/llama3.2","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"read file"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleOllamaMessages(rec, req, upstream.URL)

	// Parse SSE and verify every input_json_delta carries JSON-valid partial_json.
	sawDelta := false
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		delta, _ := ev["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if t, _ := delta["type"].(string); t != "input_json_delta" {
			continue
		}
		pj, _ := delta["partial_json"].(string)
		if !json.Valid([]byte(pj)) {
			t.Fatalf("input_json_delta partial_json is not valid JSON: %q", pj)
		}
		sawDelta = true
	}
	if !sawDelta {
		t.Fatalf("no input_json_delta event emitted; body=%s", rec.Body.String())
	}
	// Also verify stop_reason=tool_use in the message_delta
	if !strings.Contains(rec.Body.String(), `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason missing or wrong; body=%s", rec.Body.String())
	}
}
