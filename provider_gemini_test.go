package main

// provider_gemini_test.go — regression tests for every Gemini integration pitfall
// we hit during bring-up. Each test documents the exact upstream 400/429 symptom
// that the corresponding proxy fix addresses, so future Gemini-side changes or
// Claude Code tool-schema shifts surface as a failing test instead of a live
// 400 in a user session.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// ── geminiSanitizeSchema ─────────────────────────────────────────────────────

func TestGeminiSanitizeSchema_DropsUnknownKeys(t *testing.T) {
	// Symptom: Gemini 400 "Unknown name \"X\" ... Cannot find field" when
	// input_schema contains JSON Schema 2019+ keys or vendor extensions.
	in := map[string]any{
		"type":                       "object",
		"$schema":                    "http://json-schema.org/draft-07/schema#",
		"$id":                        "foo",
		"$ref":                       "#/defs/Bar",
		"definitions":                map[string]any{},
		"additionalProperties":       false,
		"x-google-enum-descriptions": []any{"desc"},
		"description":                "keep me",
		"properties": map[string]any{
			"q": map[string]any{
				"type":             "number",
				"exclusiveMinimum": 0.0,
				"propertyNames":    map[string]any{"pattern": "^.*$"},
			},
		},
	}
	out := geminiSanitizeSchema(in)

	for _, bad := range []string{"$schema", "$id", "$ref", "definitions", "additionalProperties", "x-google-enum-descriptions"} {
		if _, ok := out[bad]; ok {
			t.Errorf("key %q should have been dropped", bad)
		}
	}
	if out["description"] != "keep me" {
		t.Errorf("description lost: %#v", out["description"])
	}
	if out["type"] != "object" {
		t.Errorf("type lost: %#v", out["type"])
	}
	// Nested sanitize
	qProp := out["properties"].(map[string]any)["q"].(map[string]any)
	for _, bad := range []string{"exclusiveMinimum", "propertyNames"} {
		if _, ok := qProp[bad]; ok {
			t.Errorf("nested key %q should have been dropped", bad)
		}
	}
	if qProp["type"] != "number" {
		t.Errorf("nested type lost: %#v", qProp["type"])
	}
}

func TestGeminiSanitizeSchema_ConstBecomesEnum(t *testing.T) {
	// Symptom: Gemini 400 "Unknown name \"const\"". const is semantically
	// enum-with-one-value; convert instead of dropping so the constraint
	// survives.
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"type":  "string",
				"const": "shell",
			},
		},
	}
	out := geminiSanitizeSchema(in)
	modeProp := out["properties"].(map[string]any)["mode"].(map[string]any)
	if _, ok := modeProp["const"]; ok {
		t.Error("const should have been rewritten, not kept")
	}
	enum, ok := modeProp["enum"].([]any)
	if !ok || len(enum) != 1 || enum[0] != "shell" {
		t.Errorf("const→enum conversion failed, got %#v", modeProp["enum"])
	}
}

func TestGeminiSanitizeSchema_RequiredAlignedWithProperties(t *testing.T) {
	// Symptom: Gemini 400 "required fields ['prompt'] are not defined in the
	// schema properties." Happens when Claude Code hands us a tool schema
	// whose `required` list references names not in `properties` (buggy skill
	// definitions, or fields the sanitizer stripped). Drop orphan required
	// entries rather than forwarding the mismatch.
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
		},
		"required": []any{"command", "prompt", "nonexistent"},
	}
	out := geminiSanitizeSchema(in)
	req, ok := out["required"].([]any)
	if !ok {
		t.Fatalf("required missing entirely: %#v", out["required"])
	}
	if len(req) != 1 || req[0] != "command" {
		t.Errorf("required not filtered to {command}: %#v", req)
	}
}

func TestGeminiSanitizeSchema_RequiredWithoutPropertiesDropped(t *testing.T) {
	// Same 400 as above, degenerate variant: required exists but there is no
	// properties map at all. Drop required so Gemini doesn't complain.
	in := map[string]any{
		"type":     "object",
		"required": []any{"prompt"},
	}
	out := geminiSanitizeSchema(in)
	if _, ok := out["required"]; ok {
		t.Errorf("required should be dropped when properties absent: %#v", out["required"])
	}
}

func TestGeminiSanitizeSchema_DefaultsTypeObject(t *testing.T) {
	// Gemini 400 if properties is set but no explicit type. Auto-fill.
	in := map[string]any{
		"properties": map[string]any{
			"x": map[string]any{"type": "string"},
		},
	}
	out := geminiSanitizeSchema(in)
	if out["type"] != "object" {
		t.Errorf("type not auto-set: %#v", out["type"])
	}
}

func TestGeminiSanitizeSchema_NilSafe(t *testing.T) {
	if got := geminiSanitizeSchema(nil); got != nil {
		t.Errorf("nil in must give nil out, got %#v", got)
	}
}

// ── geminiTranslateMessages ─────────────────────────────────────────────────

func TestGeminiTranslateMessages_RoleRemap(t *testing.T) {
	msgs := []geminiAnthropicMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hey"},
	}
	out := geminiTranslateMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("want 2 contents, got %d", len(out))
	}
	if out[0].Role != "user" || out[1].Role != "model" {
		t.Errorf("role map wrong: %q,%q (want user,model)", out[0].Role, out[1].Role)
	}
}

func TestGeminiTranslateMessages_ToolResultNameRecovered(t *testing.T) {
	// Symptom: subagent completes but returns empty output because Gemini
	// silently drops functionResponses whose name doesn't match any prior
	// functionCall.name. Anthropic tool_result only carries tool_use_id, so we
	// must forward-pass the tool_use blocks and look the name back up.
	msgs := []geminiAnthropicMessage{
		{Role: "user", Content: "run an agent"},
		{Role: "assistant", Content: []any{
			map[string]any{"type": "text", "text": "sure"},
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_abc123",
				"name":  "Task",
				"input": map[string]any{"task": "echo hello"},
			},
		}},
		{Role: "user", Content: []any{
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": "toolu_abc123",
				"content":     "hello",
			},
		}},
	}
	out := geminiTranslateMessages(msgs)

	// Find the functionResponse emitted and assert its name.
	var fr *geminiFunctionResponse
	for _, c := range out {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				fr = p.FunctionResponse
			}
		}
	}
	if fr == nil {
		t.Fatal("expected a functionResponse part, got none")
	}
	if fr.Name != "Task" {
		t.Errorf("functionResponse.name = %q, want %q (Gemini drops mismatched names silently)", fr.Name, "Task")
	}
	if fr.ID != "toolu_abc123" {
		t.Errorf("functionResponse.id lost: %q", fr.ID)
	}
	if fr.Response["output"] != "hello" {
		t.Errorf("response.output lost: %#v", fr.Response)
	}
}

func TestGeminiTranslateMessages_OrphanToolResultFallback(t *testing.T) {
	// If somehow tool_result has no matching tool_use in history (resume edge
	// cases), don't crash or drop silently — give it a placeholder name so
	// Gemini at least reports the mismatch clearly.
	msgs := []geminiAnthropicMessage{
		{Role: "user", Content: []any{
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": "toolu_missing",
				"content":     "orphan",
			},
		}},
	}
	out := geminiTranslateMessages(msgs)
	if len(out) == 0 {
		t.Fatal("orphan tool_result silently dropped")
	}
	fr := out[0].Parts[0].FunctionResponse
	if fr == nil || fr.Name == "" {
		t.Fatal("orphan tool_result produced empty functionResponse")
	}
}

func TestGeminiTranslateMessages_ThoughtSignatureOnFirstFuncCall(t *testing.T) {
	// Symptom: Gemini 400 "function call X in the N. content block is missing
	// a thought_signature". Gemini 3 thinking models require the first
	// functionCall in each model turn to carry a signature. We inject the
	// official bypass sentinel.
	msgs := []geminiAnthropicMessage{
		{Role: "user", Content: "do two things"},
		{Role: "assistant", Content: []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"cmd": "ls"}},
			map[string]any{"type": "tool_use", "id": "t2", "name": "Bash", "input": map[string]any{"cmd": "pwd"}},
		}},
	}
	out := geminiTranslateMessages(msgs)

	// Locate the model turn
	var modelContent *geminiContent
	for i := range out {
		if out[i].Role == "model" {
			modelContent = &out[i]
			break
		}
	}
	if modelContent == nil {
		t.Fatal("no model content emitted")
	}
	if len(modelContent.Parts) < 2 {
		t.Fatalf("want 2 functionCall parts, got %d", len(modelContent.Parts))
	}

	// First functionCall must carry the bypass sentinel.
	if modelContent.Parts[0].ThoughtSignature != geminiSyntheticThoughtSignature {
		t.Errorf("first functionCall missing thoughtSignature, got %q", modelContent.Parts[0].ThoughtSignature)
	}
	// Second one must NOT (gemini-cli rule: only first per turn).
	if modelContent.Parts[1].ThoughtSignature != "" {
		t.Errorf("second functionCall should not have thoughtSignature, got %q", modelContent.Parts[1].ThoughtSignature)
	}
}

func TestGeminiTranslateMessages_ThoughtSignatureNotOnUserRole(t *testing.T) {
	// The bypass is a model-turn concern; never leak it to user/function_response
	// parts (Gemini would reject).
	msgs := []geminiAnthropicMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{}},
		}},
		{Role: "user", Content: []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
		}},
	}
	out := geminiTranslateMessages(msgs)
	for _, c := range out {
		if c.Role == "user" {
			for _, p := range c.Parts {
				if p.ThoughtSignature != "" {
					t.Errorf("thoughtSignature leaked into user-role part: %#v", p)
				}
			}
		}
	}
}

// ── Overage-eligible credit wiring ───────────────────────────────────────────

func TestGeminiOverageEligibleModels_MatchesGeminiCliSet(t *testing.T) {
	// Keep this list in sync with gemini-cli's OVERAGE_ELIGIBLE_MODELS
	// (billing.ts). Without these flags, 3-preview requests hit the free-tier
	// cap and 429 with "reset after 24h" instead of drawing from G1 credits.
	want := []string{
		"gemini-3-pro-preview",
		"gemini-3.1-pro-preview",
		"gemini-3-flash-preview",
	}
	for _, m := range want {
		if !geminiOverageEligibleModels[m] {
			t.Errorf("expected %s to be overage-eligible", m)
		}
	}
	// Negative checks: GA models must NOT be in the overage set (they use
	// standard tier quota, and setting credit types is a no-op or bug).
	for _, m := range []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite"} {
		if geminiOverageEligibleModels[m] {
			t.Errorf("GA model %s must not be in overage set", m)
		}
	}
}

// ── Error extraction helpers ─────────────────────────────────────────────────

func TestGeminiExtractErrorMessage(t *testing.T) {
	body := []byte(`{
		"error": {
			"code": 429,
			"message": "You have exhausted your capacity on this model. Your quota will reset after 38s.",
			"status": "RESOURCE_EXHAUSTED"
		}
	}`)
	msg := geminiExtractErrorMessage(body)
	if !strings.Contains(msg, "reset after 38s") {
		t.Errorf("message extraction failed: %q", msg)
	}

	// Non-JSON body must not panic; must return empty for fallback.
	if got := geminiExtractErrorMessage([]byte("<html>502 bad gateway</html>")); got != "" {
		t.Errorf("non-JSON should yield empty, got %q", got)
	}
}

func TestGeminiParseResetSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"Your quota will reset after 38s.", 38},
		{"something reset after 0s lol", 0},
		{"capacity exhausted. Your quota will reset after 2400s.", 2400},
		{"no hint here", 0},
		{"", 0},
	}
	for _, c := range cases {
		got := geminiParseResetSeconds(c.in)
		if got != c.want {
			t.Errorf("parseResetSeconds(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ── Tools translation smoke test ────────────────────────────────────────────

func TestGeminiTranslateTools_WrapsAsSingleEntry(t *testing.T) {
	// Gemini expects one Tool entry containing many functionDeclarations —
	// not one Tool per function. Getting this wrong usually manifests as the
	// first tool working and the rest being silently ignored.
	in := []any{
		map[string]any{"name": "A", "description": "a", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "B", "description": "b", "input_schema": map[string]any{"type": "object"}},
	}
	out := geminiTranslateTools(in)
	if len(out) != 1 {
		t.Fatalf("want 1 Tool, got %d", len(out))
	}
	if len(out[0].FunctionDeclarations) != 2 {
		t.Errorf("want 2 declarations, got %d", len(out[0].FunctionDeclarations))
	}
	names := []string{out[0].FunctionDeclarations[0].Name, out[0].FunctionDeclarations[1].Name}
	want := []string{"A", "B"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("declaration names = %v, want %v", names, want)
	}
}

func TestGeminiTranslateTools_SkipsUnnamed(t *testing.T) {
	in := []any{
		map[string]any{"description": "orphan"},
		map[string]any{"name": "Valid", "description": "ok", "input_schema": map[string]any{"type": "object"}},
	}
	out := geminiTranslateTools(in)
	if len(out) != 1 || len(out[0].FunctionDeclarations) != 1 {
		t.Fatalf("unnamed tool not filtered: %#v", out)
	}
	if out[0].FunctionDeclarations[0].Name != "Valid" {
		t.Errorf("wrong tool survived: %q", out[0].FunctionDeclarations[0].Name)
	}
}

// ── JSON marshalling of request envelope ────────────────────────────────────

func TestGeminiCARequest_CreditTypesOmittedByDefault(t *testing.T) {
	// omitempty is load-bearing: passing empty enabled_credit_types to the
	// server has unexpected side effects (route to wrong billing path). The
	// struct tag must cover both nil and empty slice cases.
	r := geminiCARequest{
		Model:   "gemini-2.5-pro",
		Project: "proj",
		Request: geminiVertexRequest{},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "enabled_credit_types") {
		t.Errorf("GA request must not include enabled_credit_types, got: %s", b)
	}
}

func TestGeminiCARequest_CreditTypesEmittedWhenSet(t *testing.T) {
	r := geminiCARequest{
		Model:              "gemini-3-pro-preview",
		Project:            "proj",
		Request:            geminiVertexRequest{},
		EnabledCreditTypes: []string{geminiCreditTypeG1},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"enabled_credit_types":["GOOGLE_ONE_AI"]`) {
		t.Errorf("preview request must include G1 credit type, got: %s", b)
	}
}
