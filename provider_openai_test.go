package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMapCodexStopReason(t *testing.T) {
	tests := []struct {
		name             string
		finishReason     string
		incompleteReason string
		hasToolUse       bool
		want             string
	}{
		{"plain completion → end_turn", "", "", false, "end_turn"},
		{"tool call completion → tool_use", "", "", true, "tool_use"},
		{"finish_reason=tool_calls → tool_use", "tool_calls", "", false, "tool_use"},
		{"finish_reason=length → max_tokens", "length", "", false, "max_tokens"},
		{"finish_reason=content_filter → stop_sequence", "content_filter", "", false, "stop_sequence"},
		{"incomplete max_output_tokens overrides tool_use", "", "max_output_tokens", true, "max_tokens"},
		{"incomplete content_filter wins over finish stop", "stop", "content_filter", false, "stop_sequence"},
		{"incomplete unknown falls back to finish_reason", "length", "something_weird", false, "max_tokens"},
		{"incomplete unknown with tool falls back to tool_use", "", "something_weird", true, "tool_use"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapCodexStopReason(tc.finishReason, tc.incompleteReason, tc.hasToolUse)
			if got != tc.want {
				t.Fatalf("mapCodexStopReason(%q, %q, %v) = %q, want %q",
					tc.finishReason, tc.incompleteReason, tc.hasToolUse, got, tc.want)
			}
		})
	}
}

func TestCodexValidatedToolArgs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty → {}", "", "{}"},
		{"valid JSON passes through", `{"path":"/tmp/x"}`, `{"path":"/tmp/x"}`},
		{"malformed wrapped as raw", `{"path":`, `{"raw":"{\"path\":"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := codexValidatedToolArgs(tc.in)
			if got != tc.want {
				t.Fatalf("codexValidatedToolArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("result not valid JSON: %q", got)
			}
		})
	}
}

func TestCodexVerbosityFor(t *testing.T) {
	tests := []struct {
		maxTokens int
		want      string
	}{
		{0, "medium"},
		{-1, "medium"},
		{64, "low"},
		{511, "low"},
		{512, "medium"},
		{2048, "medium"},
		{4096, "medium"},
		{4097, "high"},
		{32000, "high"},
	}
	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			got := codexVerbosityFor(tc.maxTokens)
			if got != tc.want {
				t.Fatalf("codexVerbosityFor(%d) = %q, want %q", tc.maxTokens, got, tc.want)
			}
		})
	}
}

// startMockCodexUpstream returns a test server that emits the given SSE events
// as Codex Responses API would. Events are joined with blank lines automatically.
func startMockCodexUpstream(t *testing.T, events []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, ev := range events {
			_, _ = w.Write([]byte("data: " + ev + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
}

// fakeCodexSession bypasses ensureCodexTokenFresh's JWT expiry check by using
// a hand-crafted JWT with an exp far in the future.
//
// IMPORTANT: decodeJWTExp uses base64.RawURLEncoding and appends `=` padding
// before decoding, which fails if the payload isn't a multiple of 4 chars.
// We deliberately pick a payload whose base64 length is already 24 chars
// (no padding needed) so the expiry check succeeds.
func fakeCodexSession() *codexSession {
	// {"alg":"none"} → header
	header := "eyJhbGciOiJub25lIn0"
	// {"exp":9999999999} → payload (exp = year 2286; base64 length 24, no pad)
	payload := "eyJleHAiOjk5OTk5OTk5OTl9"
	sig := "sig"
	return &codexSession{
		AccessToken:  header + "." + payload + "." + sig,
		RefreshToken: "refresh",
		AccountID:    "acct",
	}
}

func TestHandleCodexMessages_StreamingToolCallValidated(t *testing.T) {
	// Simulate a streaming function_call: upstream emits output_item.added,
	// then several function_call_arguments.delta, then .done with full args.
	events := []string{
		`{"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","call_id":"call_abc","name":"read_file"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"path\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"\"/tmp/x\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"item_1","arguments":"{\"path\":\"/tmp/x\"}"}`,
		`{"type":"response.completed","response":{"usage":{"output_tokens":12}}}`,
	}
	upstream := startMockCodexUpstream(t, events)
	defer upstream.Close()

	body := `{"model":"gpt-4.1","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"read it"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleCodexMessages(rec, req, fakeCodexSession(), upstream.URL, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// Walk the SSE events. Expect:
	//  - content_block_start (tool_use)
	//  - content_block_delta (input_json_delta with valid JSON)
	//  - content_block_stop
	//  - message_delta with stop_reason=tool_use
	var sawStart, sawValidDelta, sawStop, sawMsgDelta bool
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "content_block_start":
			cb, _ := ev["content_block"].(map[string]any)
			if cb != nil {
				if bt, _ := cb["type"].(string); bt == "tool_use" {
					sawStart = true
					if id, _ := cb["id"].(string); id != "toolu_call_abc" {
						t.Fatalf("tool_use id = %q, want toolu_call_abc", id)
					}
				}
			}
		case "content_block_delta":
			delta, _ := ev["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if dt, _ := delta["type"].(string); dt == "input_json_delta" {
				pj, _ := delta["partial_json"].(string)
				if !json.Valid([]byte(pj)) {
					t.Fatalf("input_json_delta is not valid JSON: %q", pj)
				}
				// Verify the content is the assembled JSON, not a half chunk
				var parsed map[string]any
				_ = json.Unmarshal([]byte(pj), &parsed)
				if p, _ := parsed["path"].(string); p != "/tmp/x" {
					t.Fatalf("assembled args wrong: %q (parsed=%v)", pj, parsed)
				}
				sawValidDelta = true
			}
		case "content_block_stop":
			sawStop = true
		case "message_delta":
			d, _ := ev["delta"].(map[string]any)
			if sr, _ := d["stop_reason"].(string); sr == "tool_use" {
				sawMsgDelta = true
			}
		}
	}
	if !sawStart || !sawValidDelta || !sawStop || !sawMsgDelta {
		t.Fatalf("missing events: start=%v delta=%v stop=%v msgDelta=%v; body=%s",
			sawStart, sawValidDelta, sawStop, sawMsgDelta, rec.Body.String())
	}
}

func TestHandleCodexMessages_IncompleteMaxOutputTokensMapsMaxTokens(t *testing.T) {
	events := []string{
		`{"type":"response.output_text.delta","delta":"truncated..."}`,
		`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"output_tokens":7}}}`,
	}
	upstream := startMockCodexUpstream(t, events)
	defer upstream.Close()

	body := `{"model":"gpt-4.1","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleCodexMessages(rec, req, fakeCodexSession(), upstream.URL, "")

	// Find message_delta's stop_reason
	var stopReason string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if t, _ := ev["type"].(string); t == "message_delta" {
			if d, ok := ev["delta"].(map[string]any); ok {
				stopReason, _ = d["stop_reason"].(string)
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens (incomplete=max_output_tokens); body=%s",
			stopReason, rec.Body.String())
	}
}

func TestHandleCodexMessages_UpstreamFailedEmitsErrorEvent(t *testing.T) {
	events := []string{
		`{"type":"response.output_text.delta","delta":"partial..."}`,
		`{"type":"response.failed","response":{"error":{"message":"model is overloaded"}}}`,
	}
	upstream := startMockCodexUpstream(t, events)
	defer upstream.Close()

	body := `{"model":"gpt-4.1","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleCodexMessages(rec, req, fakeCodexSession(), upstream.URL, "")

	out := rec.Body.String()
	var sawError bool
	var errMsg string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if t, _ := ev["type"].(string); t == "error" {
			sawError = true
			if errObj, ok := ev["error"].(map[string]any); ok {
				errMsg, _ = errObj["message"].(string)
			}
		}
	}
	if !sawError {
		t.Fatalf("expected error SSE event, got body=%s", out)
	}
	if errMsg != "model is overloaded" {
		t.Fatalf("error message = %q, want %q", errMsg, "model is overloaded")
	}
	// Partial text block should still be closed cleanly (no leaked open block)
	if strings.Count(out, `"type":"content_block_start"`) != strings.Count(out, `"type":"content_block_stop"`) {
		t.Fatalf("content_block_start / content_block_stop counts mismatch; body=%s", out)
	}
}

func TestHandleCodexMessages_FallbackOutputItemDoneStillWorks(t *testing.T) {
	// Older Codex backends emit only output_item.done with full arguments,
	// skipping output_item.added / function_call_arguments.delta. Verify the
	// fallback path still produces a well-formed tool_use block.
	events := []string{
		`{"type":"response.output_item.done","item":{"id":"item_legacy","type":"function_call","call_id":"call_z","name":"grep","arguments":"{\"pattern\":\"todo\"}"}}`,
		`{"type":"response.completed","response":{"usage":{"output_tokens":4}}}`,
	}
	upstream := startMockCodexUpstream(t, events)
	defer upstream.Close()

	body := `{"model":"gpt-4.1","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"grep it"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleCodexMessages(rec, req, fakeCodexSession(), upstream.URL, "")

	out := rec.Body.String()
	if !strings.Contains(out, `"toolu_call_z"`) {
		t.Fatalf("tool_use id missing; body=%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason != tool_use; body=%s", out)
	}
	if strings.Count(out, `"type":"content_block_start"`) != strings.Count(out, `"type":"content_block_stop"`) {
		t.Fatalf("block start/stop mismatch; body=%s", out)
	}
}

// TestCodexTranslateTools_StripsAnthropicServerTools verifies we don't forward
// Anthropic server-side tools to Codex — upstream can't execute them and an
// empty-params function spec triggers a 400 on /codex/responses.
func TestCodexTranslateTools_StripsAnthropicServerTools(t *testing.T) {
	in := []any{
		map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 5},
		map[string]any{"type": "bash_20250124", "name": "bash"},
		map[string]any{"type": "text_editor_20250124", "name": "str_replace_editor"},
		map[string]any{"type": "computer_20250124", "name": "computer", "display_width_px": 1024},
		// Client tools — should survive.
		map[string]any{"type": "custom", "name": "CustomA", "description": "a",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}}},
		map[string]any{"name": "LegacyB", "description": "old",
			"input_schema": map[string]any{"type": "object"}},
	}
	out := codexTranslateTools(in)
	if len(out) != 2 {
		t.Fatalf("want 2 surviving tools, got %d: %+v", len(out), out)
	}
	got := map[string]bool{}
	for _, spec := range out {
		got[spec.Name] = true
		if spec.Type != "function" {
			t.Errorf("tool %q type=%q, want \"function\"", spec.Name, spec.Type)
		}
		if spec.Parameters == nil {
			t.Errorf("tool %q has nil Parameters (Codex will 400)", spec.Name)
		}
	}
	for _, want := range []string{"CustomA", "LegacyB"} {
		if !got[want] {
			t.Errorf("missing surviving tool %q", want)
		}
	}
}

// TestHandleCodexMessages_ReasoningSummaryTranslatedToThinking verifies the
// reasoning translation path: a `response.reasoning_summary_text.delta` event
// upstream should surface as a thinking content block in the Anthropic-shape
// response stream. This is the diagnostic the daily notes tracked as
// "❌ thinking/reasoning block 丢失" — before the fix, summary deltas were
// dropped and CC saw zero thinking content.
func TestHandleCodexMessages_ReasoningSummaryTranslatedToThinking(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","item":{"id":"item_r1","type":"reasoning"}}`,
		`{"type":"response.reasoning_summary_part.added","item_id":"item_r1","summary_index":0}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"item_r1","delta":"Thinking about the question."}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"item_r1","delta":" Next, I will plan."}`,
		`{"type":"response.reasoning_summary_text.done","item_id":"item_r1","text":"Thinking about the question. Next, I will plan."}`,
		`{"type":"response.output_text.delta","delta":"Hello"}`,
		`{"type":"response.completed","response":{"usage":{"output_tokens":5}}}`,
	}
	upstream := startMockCodexUpstream(t, events)
	defer upstream.Close()

	body := `{"model":"gpt-5.4","stream":true,"max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleCodexMessages(rec, req, fakeCodexSession(), upstream.URL, "")

	out := rec.Body.String()
	// A thinking content_block_start must appear before the text block.
	thinkingIdx := strings.Index(out, `"type":"thinking"`)
	textIdx := strings.Index(out, `"type":"text"`)
	if thinkingIdx < 0 {
		t.Fatalf("no thinking block emitted; body=%s", out)
	}
	if textIdx < 0 {
		t.Fatalf("no text block emitted; body=%s", out)
	}
	if thinkingIdx >= textIdx {
		t.Fatalf("thinking block must precede text (thinking@%d text@%d); body=%s", thinkingIdx, textIdx, out)
	}
	// At least one thinking_delta must contain our reasoning text.
	if !strings.Contains(out, `"thinking_delta"`) {
		t.Fatalf("no thinking_delta in stream; body=%s", out)
	}
	if !strings.Contains(out, "Thinking about the question.") {
		t.Fatalf("reasoning text lost; body=%s", out)
	}
	// All blocks should be balanced.
	if a, b := strings.Count(out, `"type":"content_block_start"`), strings.Count(out, `"type":"content_block_stop"`); a != b {
		t.Fatalf("block start/stop mismatch start=%d stop=%d; body=%s", a, b, out)
	}
}

// TestAnthropicThinkingToCodexReasoning covers the thinking→effort mapping.
// Keep these thresholds in sync with anthropicThinkingToGeminiConfig — the two
// helpers share bucket boundaries and drift between them confuses users.
func TestAnthropicThinkingToCodexReasoning(t *testing.T) {
	cases := []struct {
		name       string
		in         *codexAnthropicThinking
		wantEffort string
		wantSumm   string
	}{
		{"nil → minimal, no summary", nil, "minimal", ""},
		{"disabled → minimal, no summary", &codexAnthropicThinking{Type: "disabled"}, "minimal", ""},
		{"empty type → minimal (defensive)", &codexAnthropicThinking{}, "minimal", ""},
		{"adaptive → high + auto", &codexAnthropicThinking{Type: "adaptive"}, "high", "auto"},
		{"enabled no budget → empty effort + auto", &codexAnthropicThinking{Type: "enabled"}, "", "auto"},
		{"enabled budget=1 → low", &codexAnthropicThinking{Type: "enabled", BudgetTokens: 1}, "low", "auto"},
		{"enabled budget=2047 → low (edge)", &codexAnthropicThinking{Type: "enabled", BudgetTokens: 2047}, "low", "auto"},
		{"enabled budget=2048 → medium (edge)", &codexAnthropicThinking{Type: "enabled", BudgetTokens: 2048}, "medium", "auto"},
		{"enabled budget=7999 → medium (CC haiku)", &codexAnthropicThinking{Type: "enabled", BudgetTokens: 7999}, "medium", "auto"},
		{"enabled budget=9999 → medium (edge)", &codexAnthropicThinking{Type: "enabled", BudgetTokens: 9999}, "medium", "auto"},
		{"enabled budget=10000 → high (edge)", &codexAnthropicThinking{Type: "enabled", BudgetTokens: 10000}, "high", "auto"},
		{"enabled budget=31999 → high (CC haiku)", &codexAnthropicThinking{Type: "enabled", BudgetTokens: 31999}, "high", "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := anthropicThinkingToCodexReasoning(tc.in)
			if got == nil {
				t.Fatalf("nil config")
			}
			if got.Effort != tc.wantEffort {
				t.Errorf("Effort = %q, want %q", got.Effort, tc.wantEffort)
			}
			if got.Summary != tc.wantSumm {
				t.Errorf("Summary = %q, want %q", got.Summary, tc.wantSumm)
			}
		})
	}
}

// TestCodexTranslateTools_BackfillsEmptyParams ensures tools missing an
// input_schema get a valid empty-object schema so Codex accepts them.
func TestCodexTranslateTools_BackfillsEmptyParams(t *testing.T) {
	in := []any{
		map[string]any{"name": "NoSchema", "description": "zero-arg"},
	}
	out := codexTranslateTools(in)
	if len(out) != 1 {
		t.Fatalf("want 1 tool, got %d", len(out))
	}
	if out[0].Parameters == nil {
		t.Fatalf("Parameters is nil; want backfilled schema")
	}
	if typ, _ := out[0].Parameters["type"].(string); typ != "object" {
		t.Errorf("Parameters.type = %v, want \"object\"", out[0].Parameters["type"])
	}
}
