package main

// provider_gemini.go — embedded Anthropic→Gemini (Code Assist) protocol proxy
//
// When a provider has Type=="gemini", injectProviderEnv starts a local HTTP
// server that translates Anthropic /v1/messages requests to Google Code Assist
// /v1internal:streamGenerateContent. Claude Code talks Anthropic protocol; the
// proxy silently translates to Gemini via OAuth-personal credentials (same
// credentials used by gemini-cli).
//
// Auth source: ~/.gemini/oauth_creds.json (written by `gemini` first-run login).
// Project: loadCodeAssist on first request caches cloudaicompanionProject.
// OAuth client: public installed-app credentials from the gemini-cli source.

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
	"strings"
	"sync"
	"time"
)

const (
	geminiCodeAssistEndpoint = "https://cloudcode-pa.googleapis.com"
	geminiCodeAssistVersion  = "v1internal"
	geminiOAuthTokenURL      = "https://oauth2.googleapis.com/token"
	geminiAuthFileDefault    = "~/.gemini/oauth_creds.json"

	// Public installed-app OAuth credentials from gemini-cli source. These are
	// not real secrets — Google documents that installed apps embed the client
	// secret in binaries. See:
	// https://developers.google.com/identity/protocols/oauth2#installed
	geminiClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	geminiClientSecret = "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"
)

// ── Auth ──────────────────────────────────────────────────────────────────────

type geminiAuthJSON struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	IDToken      string `json:"id_token"`
	ExpiryDate   int64  `json:"expiry_date"` // milliseconds since epoch
}

type geminiSession struct {
	mu           sync.Mutex
	AccessToken  string
	RefreshToken string
	ExpiryMillis int64  // ms since epoch
	ProjectID    string // cached after first loadCodeAssist
}

func loadGeminiAuth(authFile string) (*geminiSession, error) {
	if authFile == "" {
		authFile = geminiAuthFileDefault
	}
	path := expandPath(authFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (run: gemini, then complete OAuth login)", authFile, err)
	}
	var f geminiAuthJSON
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode gemini auth: %w", err)
	}
	if f.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in %s", authFile)
	}
	return &geminiSession{
		AccessToken:  f.AccessToken,
		RefreshToken: f.RefreshToken,
		ExpiryMillis: f.ExpiryDate,
	}, nil
}

// reloadGeminiAuthFromDisk re-reads the auth file; returns true if an update occurred.
// Handles the case where gemini-cli rotated the token out-of-band.
func reloadGeminiAuthFromDisk(sess *geminiSession, authFile string) bool {
	fresh, err := loadGeminiAuth(authFile)
	if err != nil {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if fresh.AccessToken == sess.AccessToken {
		return false
	}
	sess.AccessToken = fresh.AccessToken
	if fresh.RefreshToken != "" {
		sess.RefreshToken = fresh.RefreshToken
	}
	sess.ExpiryMillis = fresh.ExpiryMillis
	return true
}

// writeGeminiAuthToDisk persists a refreshed token back to oauth_creds.json so
// gemini-cli and subsequent proxy restarts see the fresh token. Failure is
// non-fatal (the in-memory session still works for this proxy).
func writeGeminiAuthToDisk(sess *geminiSession, authFile string) {
	if authFile == "" {
		authFile = geminiAuthFileDefault
	}
	path := expandPath(authFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	raw["access_token"] = sess.AccessToken
	if sess.RefreshToken != "" {
		raw["refresh_token"] = sess.RefreshToken
	}
	if sess.ExpiryMillis > 0 {
		raw["expiry_date"] = sess.ExpiryMillis
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".weiran.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

type geminiTokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
}

func refreshGeminiToken(sess *geminiSession, authFile string) error {
	sess.mu.Lock()
	rt := sess.RefreshToken
	sess.mu.Unlock()
	if rt == "" {
		return fmt.Errorf("no refresh_token in session")
	}
	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("client_id", geminiClientID)
	values.Set("client_secret", geminiClientSecret)
	values.Set("refresh_token", rt)

	req, err := http.NewRequest(http.MethodPost, geminiOAuthTokenURL, strings.NewReader(values.Encode()))
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
		// invalid_grant / revoked → try reloading from disk in case gemini-cli
		// refreshed out-of-band. Google does NOT rotate refresh_token on every
		// refresh (unlike codex), but the token can be revoked server-side.
		if bytes.Contains(body, []byte("invalid_grant")) {
			if reloadGeminiAuthFromDisk(sess, authFile) {
				fmt.Fprintf(os.Stderr, "[%s] gemini: invalid_grant → reloaded newer token from disk\n", appName)
				return nil
			}
		}
		return fmt.Errorf("refresh failed %s: %s", resp.Status, body)
	}
	var tr geminiTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("decode refresh: %w", err)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("no access_token in refresh response")
	}
	sess.mu.Lock()
	sess.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		sess.RefreshToken = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		sess.ExpiryMillis = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).UnixMilli()
	}
	sess.mu.Unlock()
	writeGeminiAuthToDisk(sess, authFile)
	return nil
}

func ensureGeminiTokenFresh(sess *geminiSession, authFile string) error {
	// Peek disk first — gemini-cli may have refreshed out-of-band.
	reloadGeminiAuthFromDisk(sess, authFile)

	sess.mu.Lock()
	exp := sess.ExpiryMillis
	sess.mu.Unlock()
	if exp > 0 && time.Now().Add(30*time.Second).UnixMilli() < exp {
		return nil
	}
	fmt.Fprintf(os.Stderr, "[%s] gemini token expired, refreshing...\n", appName)
	return refreshGeminiToken(sess, authFile)
}

// ── Code Assist: loadCodeAssist + onboardUser ────────────────────────────────

type geminiClientMetadata struct {
	IdeType      string `json:"ideType"`
	Platform     string `json:"platform"`
	PluginType   string `json:"pluginType"`
	DuetProject  string `json:"duetProject,omitempty"`
}

type geminiLoadCodeAssistReq struct {
	CloudaicompanionProject string               `json:"cloudaicompanionProject,omitempty"`
	Metadata                geminiClientMetadata `json:"metadata"`
}

type geminiTier struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	IsDefault              bool   `json:"isDefault,omitempty"`
	HasOnboardedPreviously bool   `json:"hasOnboardedPreviously,omitempty"`
}

type geminiLoadCodeAssistResp struct {
	CloudaicompanionProject string       `json:"cloudaicompanionProject,omitempty"`
	CurrentTier             *geminiTier  `json:"currentTier,omitempty"`
	AllowedTiers            []geminiTier `json:"allowedTiers,omitempty"`
	IneligibleTiers         []any        `json:"ineligibleTiers,omitempty"`
	PaidTier                *geminiTier  `json:"paidTier,omitempty"`
}

type geminiOnboardReq struct {
	TierID                  string               `json:"tierId"`
	CloudaicompanionProject string               `json:"cloudaicompanionProject,omitempty"`
	Metadata                geminiClientMetadata `json:"metadata"`
}

type geminiLongRunningOp struct {
	Name     string `json:"name"`
	Done     bool   `json:"done"`
	Response *struct {
		CloudaicompanionProject *struct {
			ID string `json:"id"`
		} `json:"cloudaicompanionProject"`
	} `json:"response,omitempty"`
}

// resolveGeminiProject calls loadCodeAssist (and onboardUser if necessary) once
// and caches the resulting cloudaicompanionProject ID on the session. Matches
// the setup.ts flow in gemini-cli.
func resolveGeminiProject(sess *geminiSession, authFile string) error {
	sess.mu.Lock()
	if sess.ProjectID != "" {
		sess.mu.Unlock()
		return nil
	}
	sess.mu.Unlock()

	if err := ensureGeminiTokenFresh(sess, authFile); err != nil {
		return fmt.Errorf("token: %w", err)
	}

	meta := geminiClientMetadata{
		IdeType:    "IDE_UNSPECIFIED",
		Platform:   "PLATFORM_UNSPECIFIED",
		PluginType: "GEMINI",
	}
	loadReq := geminiLoadCodeAssistReq{Metadata: meta}
	loadBody, _ := json.Marshal(loadReq)

	loadURL := fmt.Sprintf("%s/%s:loadCodeAssist", geminiCodeAssistEndpoint, geminiCodeAssistVersion)
	req, err := http.NewRequest(http.MethodPost, loadURL, bytes.NewReader(loadBody))
	if err != nil {
		return err
	}
	sess.mu.Lock()
	tok := sess.AccessToken
	sess.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("loadCodeAssist: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("loadCodeAssist %s: %s", resp.Status, body)
	}
	var lr geminiLoadCodeAssistResp
	if err := json.Unmarshal(body, &lr); err != nil {
		return fmt.Errorf("decode loadCodeAssist: %w", err)
	}

	// Happy path: already onboarded with a project.
	if lr.CloudaicompanionProject != "" {
		sess.mu.Lock()
		sess.ProjectID = lr.CloudaicompanionProject
		sess.mu.Unlock()
		fmt.Fprintf(os.Stderr, "[%s] gemini project: %s (tier=%s)\n",
			appName, lr.CloudaicompanionProject, tierID(lr.CurrentTier))
		return nil
	}

	// Need onboarding. Pick a tier: prefer currentTier, else first allowedTier.
	var tier *geminiTier
	if lr.CurrentTier != nil {
		tier = lr.CurrentTier
	} else {
		for i := range lr.AllowedTiers {
			if lr.AllowedTiers[i].IsDefault {
				tier = &lr.AllowedTiers[i]
				break
			}
		}
		if tier == nil && len(lr.AllowedTiers) > 0 {
			tier = &lr.AllowedTiers[0]
		}
	}
	if tier == nil || tier.ID == "" {
		return fmt.Errorf("no onboardable tier available; run `gemini` and complete first-run setup")
	}

	onboardReq := geminiOnboardReq{
		TierID:   tier.ID,
		Metadata: meta,
	}
	// Free tier: must NOT send project; free tier uses managed project.
	if tier.ID != "free-tier" {
		onboardReq.CloudaicompanionProject = lr.CloudaicompanionProject // may be empty; server accepts
	}
	onboardBody, _ := json.Marshal(onboardReq)
	onboardURL := fmt.Sprintf("%s/%s:onboardUser", geminiCodeAssistEndpoint, geminiCodeAssistVersion)
	projectID, err := geminiPollOnboard(sess, onboardURL, onboardBody)
	if err != nil {
		return err
	}
	sess.mu.Lock()
	sess.ProjectID = projectID
	sess.mu.Unlock()
	fmt.Fprintf(os.Stderr, "[%s] gemini onboarded: project=%s tier=%s\n", appName, projectID, tier.ID)
	return nil
}

func tierID(t *geminiTier) string {
	if t == nil {
		return "?"
	}
	return t.ID
}

func geminiPollOnboard(sess *geminiSession, url string, body []byte) (string, error) {
	deadline := time.Now().Add(60 * time.Second)
	curURL := url
	var curBody []byte = body
	method := http.MethodPost
	for {
		req, err := http.NewRequest(method, curURL, bytes.NewReader(curBody))
		if err != nil {
			return "", err
		}
		sess.mu.Lock()
		tok := sess.AccessToken
		sess.mu.Unlock()
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("onboard: %w", err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("onboard %s: %s", resp.Status, respBody)
		}
		var op geminiLongRunningOp
		if err := json.Unmarshal(respBody, &op); err != nil {
			return "", fmt.Errorf("decode onboard: %w", err)
		}
		if op.Done {
			if op.Response != nil && op.Response.CloudaicompanionProject != nil {
				return op.Response.CloudaicompanionProject.ID, nil
			}
			return "", fmt.Errorf("onboarding done but no project returned")
		}
		if op.Name == "" {
			return "", fmt.Errorf("onboarding pending but no operation name")
		}
		// Poll operation. Second iteration onward: GET the operation URL.
		curURL = fmt.Sprintf("%s/%s/%s", geminiCodeAssistEndpoint, geminiCodeAssistVersion, op.Name)
		method = http.MethodGet
		curBody = nil
		if time.Now().After(deadline) {
			return "", fmt.Errorf("onboarding timed out")
		}
		time.Sleep(2 * time.Second)
	}
}

// ── Anthropic request/response types (shared shape with codex proxy) ─────────

type geminiAnthropicRequest struct {
	Model         string                   `json:"model"`
	MaxTokens     int                      `json:"max_tokens"`
	Temperature   *float64                 `json:"temperature,omitempty"`
	TopP          *float64                 `json:"top_p,omitempty"`
	StopSequences []string                 `json:"stop_sequences,omitempty"`
	System        any                      `json:"system,omitempty"`
	Messages      []geminiAnthropicMessage `json:"messages"`
	Stream        bool                     `json:"stream"`
	Thinking      *geminiAnthropicThinking `json:"thinking,omitempty"`
}

// geminiAnthropicThinking mirrors codexAnthropicThinking — see that type's
// docstring for the three shapes CC emits.
type geminiAnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type geminiAnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type geminiAnthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

type geminiAnthropicResponse struct {
	ID      string                 `json:"id"`
	Type    string                 `json:"type"`
	Role    string                 `json:"role"`
	Content []geminiAnthropicBlock `json:"content"`
	Model   string                 `json:"model"`
	// StopReason/StopSequence are pointers so message_start can serialize
	// them as JSON null (Anthropic canonical) rather than "" which some
	// SDK paths treat as "already set" and never update from message_delta.
	StopReason   *string              `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage        geminiAnthropicUsage `json:"usage"`
}

type geminiAnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type geminiMsgStart struct {
	Type    string                  `json:"type"`
	Message geminiAnthropicResponse `json:"message"`
}

type geminiBlockStart struct {
	Type         string               `json:"type"`
	Index        int                  `json:"index"`
	ContentBlock geminiAnthropicBlock `json:"content_block"`
}

type geminiBlockDelta struct {
	Type  string         `json:"type"`
	Index int            `json:"index"`
	Delta map[string]any `json:"delta"`
}

type geminiBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type geminiMsgDelta struct {
	Type  string `json:"type"`
	Delta struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage geminiAnthropicUsage `json:"usage"`
}

type geminiMsgStop struct {
	Type string `json:"type"`
}

// ── Gemini (Code Assist / Vertex) request types ──────────────────────────────

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	// ThoughtSignature is required by Gemini 3 thinking models on the first
	// functionCall of every model turn in the active loop. Without it the API
	// 400s: "function call X is missing a `thought_signature`". We don't cache
	// the real signature from the streaming response (complex + adds state),
	// instead we use the official bypass constant from gemini-cli:
	// SYNTHETIC_THOUGHT_SIGNATURE = "skip_thought_signature_validator".
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

// geminiSyntheticThoughtSignature matches gemini-cli's SYNTHETIC_THOUGHT_SIGNATURE
// constant. The Gemini API recognises this sentinel and skips thought-signature
// validation on the part that carries it.
const geminiSyntheticThoughtSignature = "skip_thought_signature_validator"

type geminiFunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiFunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiGenerationConfig struct {
	Temperature     *float64              `json:"temperature,omitempty"`
	TopP            *float64              `json:"topP,omitempty"`
	MaxOutputTokens int                   `json:"maxOutputTokens,omitempty"`
	StopSequences   []string              `json:"stopSequences,omitempty"`
	ThinkingConfig  *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

// geminiThinkingConfig controls reasoning emission. We always set
// includeThoughts=true so the upstream emits parts with `thought:true` carrying
// the reasoning summary; without this Gemini 3 thinks internally but never
// surfaces a summary, so CC sees zero thinking blocks.
//
// thinkingLevel is for Gemini 3 family ("HIGH"/"MEDIUM"/"LOW"); thinkingBudget
// is for 2.5. Unset for non-thinking models — Gemini ignores both fields.
type geminiThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
}

// anthropicThinkingToGeminiConfig maps Anthropic's top-level `thinking` field
// to Gemini's thinkingConfig. Same thresholds as the Codex mapping for
// consistency — see anthropicThinkingToCodexReasoning.
//
// Model-family routing:
//   - gemini-3-*: uses thinkingLevel enum (LOW/MEDIUM/HIGH). Setting
//     thinkingBudget on Gemini 3 is rejected with 400 INVALID_ARGUMENT.
//   - gemini-2.5-*: uses thinkingBudget (integer). -1 means dynamic (Google
//     decides), 0 means thinking off (flash-lite only), positive values cap.
//
// IncludeThoughts stays true whenever thinking isn't "disabled" so reasoning
// summaries actually stream back — otherwise CC sees zero thinking blocks.
func anthropicThinkingToGeminiConfig(th *geminiAnthropicThinking, model string) *geminiThinkingConfig {
	isG3 := strings.HasPrefix(model, "gemini-3")

	// Disabled / absent → no thinking. On G2.5 this is thinkingBudget:0; on
	// G3 we just omit thinkingLevel so the model falls back to its default
	// (Gemini 3 cannot truly disable thinking via the config).
	if th == nil || th.Type == "" || th.Type == "disabled" {
		if isG3 {
			// No way to force-disable on G3; return nil so we don't send config.
			return nil
		}
		zero := 0
		return &geminiThinkingConfig{ThinkingBudget: &zero}
	}

	tc := &geminiThinkingConfig{IncludeThoughts: true}

	if th.Type == "adaptive" {
		if isG3 {
			tc.ThinkingLevel = "HIGH"
		} else {
			dynamic := -1
			tc.ThinkingBudget = &dynamic
		}
		return tc
	}

	// type == "enabled" (or unknown → treat as enabled)
	if isG3 {
		switch {
		case th.BudgetTokens <= 0:
			tc.ThinkingLevel = "HIGH" // fallback when budget unknown
		case th.BudgetTokens < 2048:
			tc.ThinkingLevel = "LOW"
		case th.BudgetTokens < 10000:
			tc.ThinkingLevel = "MEDIUM"
		default:
			tc.ThinkingLevel = "HIGH"
		}
		return tc
	}
	// Gemini 2.5: pass raw budget through. If unspecified, use the gemini-cli
	// default (8192 tokens).
	budget := th.BudgetTokens
	if budget <= 0 {
		budget = 8192
	}
	tc.ThinkingBudget = &budget
	return tc
}

type geminiVertexRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiTool            `json:"tools,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiCARequest struct {
	Model              string              `json:"model"`
	Project            string              `json:"project,omitempty"`
	UserPromptID       string              `json:"user_prompt_id,omitempty"`
	Request            geminiVertexRequest `json:"request"`
	EnabledCreditTypes []string            `json:"enabled_credit_types,omitempty"`
}

// geminiOverageEligibleModels mirrors gemini-cli's OVERAGE_ELIGIBLE_MODELS set.
// These models require enabled_credit_types=["GOOGLE_ONE_AI"] to draw from the
// paid AI Ultra credit pool; without that flag they fall back to the (tiny)
// free-tier capacity and return 429.
var geminiOverageEligibleModels = map[string]bool{
	"gemini-3-pro-preview":   true,
	"gemini-3.1-pro-preview": true,
	"gemini-3-flash-preview": true,
}

const geminiCreditTypeG1 = "GOOGLE_ONE_AI"

// ── Protocol translation: Anthropic → Gemini ─────────────────────────────────

func geminiExtractSystemText(system any) string {
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

// geminiSeenToolTypes tracks unique Anthropic tool `type` values observed on
// incoming requests. Same pattern as codexSeenToolTypes — diagnostic only.
var geminiSeenToolTypes = map[string]bool{}

// geminiTranslateTools converts Anthropic tool definitions to a single Gemini
// Tool entry with many function declarations (Gemini's preferred shape).
//
// Like the Codex path, strips Anthropic server tools (web_search_*, bash_*,
// text_editor_*, computer_*) that upstream Gemini cannot execute. Gemini has
// its own built-in tools (google_search, code_execution) but they use a
// different wire shape — we do NOT auto-map web_search_20250305 → google_search
// because CC doesn't know how to consume Gemini's groundingMetadata format.
func geminiTranslateTools(tools []any) []geminiTool {
	var decls []geminiFunctionDeclaration
	for _, t := range tools {
		obj, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := obj["name"].(string)
		desc, _ := obj["description"].(string)
		typ, _ := obj["type"].(string)
		params, _ := obj["input_schema"].(map[string]any)

		diagKey := typ + "|" + name
		if !geminiSeenToolTypes[diagKey] {
			geminiSeenToolTypes[diagKey] = true
			fmt.Fprintf(os.Stderr, "[%s] gemini tool seen: type=%q name=%q has_schema=%v\n",
				appName, typ, name, params != nil)
		}

		if name == "" {
			continue
		}
		// Strip Anthropic server tools — no upstream equivalent we can wire up.
		if typ != "" && typ != "custom" && typ != "function" {
			fmt.Fprintf(os.Stderr, "[%s] gemini: stripping Anthropic server tool type=%q name=%q (no upstream equivalent)\n",
				appName, typ, name)
			continue
		}
		// Gemini rejects extra keys like "$schema", "additionalProperties" on
		// some schemas. Copy the input_schema as-is; Gemini 2.5 is tolerant.
		sanitized := geminiSanitizeSchema(params)
		if sanitized == nil {
			sanitized = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		decls = append(decls, geminiFunctionDeclaration{
			Name:        name,
			Description: desc,
			Parameters:  sanitized,
		})
	}
	if len(decls) == 0 {
		return nil
	}
	return []geminiTool{{FunctionDeclarations: decls}}
}

// geminiAllowedSchemaKeys is the OpenAPI-3.0-subset whitelist that Gemini's
// function-calling schema validator accepts. Any other key (JSON Schema 2019+
// additions, vendor extensions, $-refs, boolean subschemas) gets dropped — the
// validator rejects unknown fields with HTTP 400 INVALID_ARGUMENT.
var geminiAllowedSchemaKeys = map[string]bool{
	"type":        true,
	"format":      true,
	"description": true,
	"nullable":    true,
	"enum":        true,
	"properties":  true,
	"required":    true,
	"items":       true,
	"minimum":     true,
	"maximum":     true,
	"maxLength":   true,
	"minLength":   true,
	"pattern":     true,
	"maxItems":    true,
	"minItems":    true,
	"default":     true,
	"title":       true,
	"example":     true,
	// anyOf / oneOf are supported on Gemini 2.5; keep them.
	"anyOf": true,
	"oneOf": true,
}

// geminiSanitizeSchema recursively filters a JSON Schema to the subset Gemini
// accepts. Converts `const` → `enum: [value]`, drops everything else unknown
// (propertyNames, exclusiveMinimum, $schema, x-google-*, additionalProperties,
// etc.), and fixes structural issues that Gemini's validator is strict about:
//   - `required` entries that don't appear in `properties` are dropped.
//   - object schemas with `properties` but no explicit `type` get `type:"object"`.
//   - `required` without `properties` is dropped (Gemini 400s on this).
//
// Critical: `properties` and nested structures have user-defined field names as
// keys (not JSON Schema keywords), so the whitelist only applies to the outer
// schema object. Values inside `properties` are themselves sub-schemas and get
// recursively sanitized; values inside `items` likewise.
func geminiSanitizeSchema(s map[string]any) map[string]any {
	if s == nil {
		return nil
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		// const → enum with single value (semantically equivalent, and Gemini
		// understands enum).
		if k == "const" {
			out["enum"] = []any{v}
			continue
		}
		if !geminiAllowedSchemaKeys[k] {
			continue
		}
		switch k {
		case "properties":
			// User-defined field names here; recurse into each value.
			if m, ok := v.(map[string]any); ok {
				sanitizedProps := make(map[string]any, len(m))
				for propName, propSchema := range m {
					if ps, ok := propSchema.(map[string]any); ok {
						sanitizedProps[propName] = geminiSanitizeSchema(ps)
					} else {
						sanitizedProps[propName] = propSchema
					}
				}
				out[k] = sanitizedProps
			} else {
				out[k] = v
			}
		case "items", "additionalItems":
			// Single sub-schema (or array of sub-schemas in older drafts).
			switch vv := v.(type) {
			case map[string]any:
				out[k] = geminiSanitizeSchema(vv)
			case []any:
				arr := make([]any, 0, len(vv))
				for _, item := range vv {
					if m, ok := item.(map[string]any); ok {
						arr = append(arr, geminiSanitizeSchema(m))
					} else {
						arr = append(arr, item)
					}
				}
				out[k] = arr
			default:
				out[k] = v
			}
		case "anyOf", "oneOf":
			// Array of sub-schemas.
			if arr, ok := v.([]any); ok {
				sanitized := make([]any, 0, len(arr))
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						sanitized = append(sanitized, geminiSanitizeSchema(m))
					} else {
						sanitized = append(sanitized, item)
					}
				}
				out[k] = sanitized
			} else {
				out[k] = v
			}
		default:
			// Leaf values (type, description, enum, minimum, etc.) pass through.
			out[k] = v
		}
	}
	// Post-pass: reconcile required vs properties. Gemini rejects schemas where
	// required references a name not declared in properties.
	if reqRaw, ok := out["required"].([]any); ok {
		propsRaw, _ := out["properties"].(map[string]any)
		if len(propsRaw) == 0 {
			// No properties at all — required is meaningless, drop it.
			delete(out, "required")
		} else {
			filtered := make([]any, 0, len(reqRaw))
			for _, r := range reqRaw {
				if name, ok := r.(string); ok {
					if _, present := propsRaw[name]; present {
						filtered = append(filtered, name)
					}
				}
			}
			if len(filtered) == 0 {
				delete(out, "required")
			} else {
				out["required"] = filtered
			}
		}
	}
	// If we have properties but no explicit type, default to "object" (Gemini
	// validator sometimes insists on it).
	if _, hasType := out["type"]; !hasType {
		if _, hasProps := out["properties"]; hasProps {
			out["type"] = "object"
		}
	}
	return out
}

// geminiTranslateMessages converts Anthropic messages (with tool_use/tool_result
// blocks) into Gemini contents. Role mapping: assistant→model, user→user.
// tool_use becomes a model-role functionCall part; tool_result becomes a
// user-role functionResponse part.
//
// Critical invariant: each functionResponse.name MUST equal the corresponding
// functionCall.name. Gemini silently drops orphaned functionResponses, which
// makes tools appear to return empty output to the model (the exact "subagent
// completed but returned no output" symptom). Anthropic's tool_result only
// carries tool_use_id, so we build a forward-pass map id→name from tool_use
// blocks and look the name back up when emitting functionResponse.
func geminiTranslateMessages(messages []geminiAnthropicMessage) []geminiContent {
	// Forward pass: map tool_use_id → tool name so tool_result blocks can
	// recover the name (Anthropic tool_result doesn't carry it).
	toolIDToName := make(map[string]string)
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		content, ok := m.Content.([]any)
		if !ok {
			continue
		}
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == "tool_use" {
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				if id != "" && name != "" {
					toolIDToName[id] = name
				}
			}
		}
	}

	var out []geminiContent
	for _, m := range messages {
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		switch content := m.Content.(type) {
		case string:
			if content == "" {
				continue
			}
			out = append(out, geminiContent{
				Role:  role,
				Parts: []geminiPart{{Text: content}},
			})
		case []any:
			var parts []geminiPart
			var toolResponseParts []geminiPart // tool_result blocks always go as user role
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := block["type"].(string)
				switch typ {
				case "text":
					if t, _ := block["text"].(string); t != "" {
						parts = append(parts, geminiPart{Text: t})
					}
				case "tool_use":
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					var args map[string]any
					if inputVal, ok := block["input"].(map[string]any); ok {
						args = inputVal
					} else {
						args = map[string]any{}
					}
					parts = append(parts, geminiPart{
						FunctionCall: &geminiFunctionCall{
							ID:   id,
							Name: name,
							Args: args,
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
					// Recover the matching function name; without this Gemini
					// silently drops the response and tools look empty.
					name := toolIDToName[toolUseID]
					if name == "" {
						name = "unknown_tool"
					}
					toolResponseParts = append(toolResponseParts, geminiPart{
						FunctionResponse: &geminiFunctionResponse{
							ID:   toolUseID,
							Name: name,
							Response: map[string]any{
								// Gemini SDK convention: wrap text under
								// `output`; model prompts are trained to read
								// this key.
								"output": outputText,
							},
						},
					})
				}
			}
			// Emit in order. If this is a user turn with tool_results, those go
			// as role=user; any stray text parts also go as user. If model turn,
			// parts (text + functionCall) go as role=model.
			if len(parts) > 0 {
				out = append(out, geminiContent{Role: role, Parts: parts})
			}
			if len(toolResponseParts) > 0 {
				out = append(out, geminiContent{Role: "user", Parts: toolResponseParts})
			}
		}
	}
	// Gemini requires non-empty contents and first message should be role=user.
	// Final pass: ensure every model turn's first functionCall has a
	// thoughtSignature, otherwise Gemini 3 thinking models 400.
	for i := range out {
		if out[i].Role != "model" {
			continue
		}
		for j := range out[i].Parts {
			if out[i].Parts[j].FunctionCall != nil {
				if out[i].Parts[j].ThoughtSignature == "" {
					out[i].Parts[j].ThoughtSignature = geminiSyntheticThoughtSignature
				}
				break // only the first functionCall needs it
			}
		}
	}
	return out
}

// ── Proxy server ─────────────────────────────────────────────────────────────

var serverGeminiProxies sync.Map

func detectServerGeminiProxy(providerName string) int {
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
		fmt.Sprintf("%s/api/proxy/gemini?provider=%s", serverURL, providerName), nil)
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
	fmt.Fprintf(os.Stderr, "[%s] reusing server gemini proxy on port %d\n", appName, result.Port)
	return result.Port
}

// startGeminiProxy starts a local HTTP server that translates Anthropic
// /v1/messages to Gemini Code Assist streamGenerateContent. Returns the port
// the server is listening on.
func startGeminiProxy(provider providerConfig) (int, error) {
	authFile := provider.AuthFile
	sess, err := loadGeminiAuth(authFile)
	if err != nil {
		return 0, fmt.Errorf("gemini auth: %w", err)
	}

	// Resolve project up front so the first real request doesn't stall.
	if err := resolveGeminiProject(sess, authFile); err != nil {
		return 0, fmt.Errorf("gemini project: %w", err)
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
		handleGeminiMessages(w, r, sess, authFile)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok","proxy":"weiran/gemini"}`)
	})

	go func() {
		if err := http.Serve(ln, mux); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] gemini proxy stopped: %v\n", appName, err)
		}
	}()

	activeOpenAIProxyPort = port // reuse the same global to signal "external proxy running" to execClaude
	fmt.Fprintf(os.Stderr, "[%s] gemini proxy started on http://127.0.0.1:%d\n", appName, port)
	return port, nil
}

// geminiExtractErrorMessage pulls the human-readable message out of Google's
// `{"error": {"code": N, "message": "...", "status": "..."}}` envelope.
// Returns empty string on any parse failure so the caller can fall back to the
// raw body.
func geminiExtractErrorMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error.Message
}

// geminiParseResetSeconds extracts the integer seconds from Google's phrasing
// "Your quota will reset after Ns." Returns 0 if no such hint is present.
func geminiParseResetSeconds(msg string) int {
	idx := strings.Index(msg, "reset after ")
	if idx < 0 {
		return 0
	}
	rest := msg[idx+len("reset after "):]
	var n int
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// Local cooldown: after an upstream 429 for model X, block subsequent
// requests for the same model for at least `geminiMinCooldownSecs` seconds
// at the proxy layer, without burning upstream capacity. This protects the
// OAuth quota window from CC's retry stampede — even if CC ignores
// Retry-After, we simply refuse locally until the window has a chance to
// reopen.
const geminiMinCooldownSecs = 90

var (
	geminiCooldownMu    sync.RWMutex
	geminiCooldownUntil = map[string]time.Time{}
)

// geminiCooldownRemaining returns the remaining cooldown for a model,
// or 0 if not cooling down.
func geminiCooldownRemaining(model string) time.Duration {
	geminiCooldownMu.RLock()
	until, ok := geminiCooldownUntil[model]
	geminiCooldownMu.RUnlock()
	if !ok {
		return 0
	}
	remaining := time.Until(until)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// geminiCooldownArm records that model X should be locked out for at
// least `seconds`. Extends an existing cooldown if longer. Always
// enforces the floor `geminiMinCooldownSecs`.
func geminiCooldownArm(model string, seconds int) {
	if seconds < geminiMinCooldownSecs {
		seconds = geminiMinCooldownSecs
	}
	newUntil := time.Now().Add(time.Duration(seconds) * time.Second)
	geminiCooldownMu.Lock()
	if cur, ok := geminiCooldownUntil[model]; !ok || newUntil.After(cur) {
		geminiCooldownUntil[model] = newUntil
	}
	geminiCooldownMu.Unlock()
}

// geminiWriteLocalCooldown replies with an Anthropic-shaped rate_limit_error
// and Retry-After header, without touching the upstream Google endpoint.
func geminiWriteLocalCooldown(w http.ResponseWriter, model string, remaining time.Duration) {
	secs := int(remaining.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	msg := fmt.Sprintf("proxy cooldown: model %s is locally rate-limited for %ds to protect upstream quota", model, secs)
	errMsg, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "rate_limit_error", "message": msg},
	})
	w.Write(errMsg)
}

func geminiWriteSSE(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(b))
}

func geminiRandomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// mapGeminiFinishReason translates Gemini's finishReason to Anthropic stop_reason.
//
//	STOP            → end_turn (or tool_use if a functionCall was emitted)
//	MAX_TOKENS      → max_tokens
//	SAFETY / RECITATION / BLOCKLIST / PROHIBITED_CONTENT / SPII → stop_sequence
//	OTHER / unknown → end_turn
func mapGeminiFinishReason(reason string, hasToolUse bool) string {
	switch reason {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "stop_sequence"
	}
	if hasToolUse {
		return "tool_use"
	}
	return "end_turn"
}

func handleGeminiMessages(w http.ResponseWriter, r *http.Request, sess *geminiSession, authFile string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var rawReq map[string]any
	if err := json.Unmarshal(body, &rawReq); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var areq geminiAnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Local cooldown check: if we recently ate a 429 for this model, block
	// at the proxy layer instead of burning upstream capacity. This keeps
	// multi-session CC from stampeding the OAuth quota window.
	if remaining := geminiCooldownRemaining(areq.Model); remaining > 0 {
		fmt.Fprintf(os.Stderr, "[%s] gemini local cooldown: blocking model=%s for %.0fs (no upstream call)\n",
			appName, areq.Model, remaining.Seconds())
		geminiWriteLocalCooldown(w, areq.Model, remaining)
		return
	}

	if err := ensureGeminiTokenFresh(sess, authFile); err != nil {
		http.Error(w, "token refresh: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if err := resolveGeminiProject(sess, authFile); err != nil {
		http.Error(w, "project setup: "+err.Error(), http.StatusUnauthorized)
		return
	}

	systemText := geminiExtractSystemText(areq.System)

	var tools []geminiTool
	if toolsRaw, ok := rawReq["tools"].([]any); ok && len(toolsRaw) > 0 {
		tools = geminiTranslateTools(toolsRaw)
	}

	contents := geminiTranslateMessages(areq.Messages)
	if len(contents) == 0 {
		http.Error(w, "no translatable messages", http.StatusBadRequest)
		return
	}

	genConfig := &geminiGenerationConfig{}
	if areq.MaxTokens > 0 {
		genConfig.MaxOutputTokens = areq.MaxTokens
	}
	if areq.Temperature != nil {
		genConfig.Temperature = areq.Temperature
	}
	if areq.TopP != nil {
		genConfig.TopP = areq.TopP
	}
	if len(areq.StopSequences) > 0 {
		genConfig.StopSequences = areq.StopSequences
	}
	// Map Anthropic's thinking config onto Gemini's thinkingConfig. Gemini
	// ignores thinkingConfig for non-thinking models, so this is safe across
	// the board. Gemini 3 uses thinkingLevel (LOW/MEDIUM/HIGH enum); Gemini
	// 2.5 uses thinkingBudget (integer token count, 0=off, -1=dynamic).
	genConfig.ThinkingConfig = anthropicThinkingToGeminiConfig(areq.Thinking, areq.Model)

	vertexReq := geminiVertexRequest{
		Contents:         contents,
		Tools:            tools,
		GenerationConfig: genConfig,
	}
	if systemText != "" {
		vertexReq.SystemInstruction = &geminiContent{
			Role:  "user",
			Parts: []geminiPart{{Text: systemText}},
		}
	}

	sess.mu.Lock()
	projectID := sess.ProjectID
	accessToken := sess.AccessToken
	sess.mu.Unlock()

	caReq := geminiCARequest{
		Model:        areq.Model,
		Project:      projectID,
		UserPromptID: "weiran-" + geminiRandomID(),
		Request:      vertexReq,
	}
	// 3-preview models must draw from the paid G1 credit pool, else they
	// hit the free-tier capacity cap and 429 immediately.
	if geminiOverageEligibleModels[areq.Model] {
		caReq.EnabledCreditTypes = []string{geminiCreditTypeG1}
	}

	reqBody, err := json.Marshal(caReq)
	if err != nil {
		http.Error(w, "marshal request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	streamURL := fmt.Sprintf("%s/%s:streamGenerateContent?alt=sse",
		geminiCodeAssistEndpoint, geminiCodeAssistVersion)

	upReq, err := http.NewRequest(http.MethodPost, streamURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "build request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	upReq.Header.Set("Authorization", "Bearer "+accessToken)
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// One retry on 401 after forcing a refresh.
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if err := refreshGeminiToken(sess, authFile); err == nil {
			sess.mu.Lock()
			accessToken = sess.AccessToken
			sess.mu.Unlock()
			upReq2, _ := http.NewRequest(http.MethodPost, streamURL, bytes.NewReader(reqBody))
			upReq2.Header.Set("Authorization", "Bearer "+accessToken)
			upReq2.Header.Set("Content-Type", "application/json")
			upReq2.Header.Set("Accept", "text/event-stream")
			resp, err = http.DefaultClient.Do(upReq2)
			if err != nil {
				http.Error(w, "upstream retry: "+err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		upBody, _ := io.ReadAll(resp.Body)
		// Log upstream error to stderr so the real reason surfaces (Claude Code
		// only shows the opaque body back; debugging without this is guesswork).
		snippet := string(upBody)
		if len(snippet) > 2000 {
			snippet = snippet[:2000] + "...[truncated]"
		}
		fmt.Fprintf(os.Stderr, "[%s] gemini upstream %d for model=%s: %s\n",
			appName, resp.StatusCode, areq.Model, snippet)

		// Pick the Anthropic error type based on HTTP status. CC / SDK clients
		// branch on `type`: rate_limit_error triggers exponential backoff, while
		// api_error gets a fast retry that compounds the upstream throttle.
		errType := "api_error"
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			errType = "rate_limit_error"
		case resp.StatusCode == http.StatusUnauthorized:
			errType = "authentication_error"
		case resp.StatusCode == http.StatusForbidden:
			errType = "permission_error"
		case resp.StatusCode == http.StatusRequestEntityTooLarge:
			errType = "request_too_large"
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			errType = "invalid_request_error"
		case resp.StatusCode >= 500:
			errType = "api_error"
		}

		// Extract a clean message from Google's nested error envelope so CC
		// doesn't show the whole JSON blob to the user.
		cleanMsg := geminiExtractErrorMessage(upBody)
		if cleanMsg == "" {
			cleanMsg = string(upBody)
		}

		// Honour rate-limit reset hint: parse "reset after Ns" out of the
		// message and emit Retry-After so CC backs off the right amount.
		// Additionally, arm a local cooldown on the proxy so concurrent
		// sessions don't stampede the upstream quota window — once we see
		// a 429, we refuse locally for at least geminiMinCooldownSecs even
		// if CC decides to ignore Retry-After.
		if resp.StatusCode == http.StatusTooManyRequests {
			parsed := geminiParseResetSeconds(cleanMsg)
			// Arm local cooldown (floor enforced inside geminiCooldownArm).
			geminiCooldownArm(areq.Model, parsed)
			remaining := geminiCooldownRemaining(areq.Model)
			retrySecs := int(remaining.Seconds())
			if retrySecs < 1 {
				retrySecs = geminiMinCooldownSecs
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retrySecs))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		errMsg, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": errType, "message": cleanMsg},
		})
		w.Write(errMsg)
		return
	}

	// Stream Gemini SSE → Anthropic SSE.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	msgID := "msg_" + geminiRandomID()
	geminiWriteSSE(w, "message_start", geminiMsgStart{
		Type: "message_start",
		Message: geminiAnthropicResponse{
			ID:      msgID,
			Type:    "message",
			Role:    "assistant",
			Content: []geminiAnthropicBlock{},
			Model:   areq.Model,
			Usage:   geminiAnthropicUsage{},
		},
	})
	geminiWriteSSE(w, "ping", map[string]string{"type": "ping"})
	if flusher != nil {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(resp.Body)
	// Gemini chunks can be large (full thinking traces); bump buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	blockIndex := 0
	textBlockStarted := false
	thinkingBlockStarted := false
	hasToolUse := false
	inputTokens := 0
	outputTokens := 0
	finalFinishReason := ""

	closeTextIfOpen := func() {
		if textBlockStarted {
			geminiWriteSSE(w, "content_block_stop", geminiBlockStop{
				Type: "content_block_stop", Index: blockIndex,
			})
			blockIndex++
			textBlockStarted = false
		}
	}
	closeThinkingIfOpen := func() {
		if thinkingBlockStarted {
			geminiWriteSSE(w, "content_block_stop", geminiBlockStop{
				Type: "content_block_stop", Index: blockIndex,
			})
			blockIndex++
			thinkingBlockStarted = false
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "" || data == "[DONE]" {
			continue
		}

		var env struct {
			Response struct {
				Candidates []struct {
					Content struct {
						Role  string `json:"role"`
						Parts []struct {
							Text         string `json:"text,omitempty"`
							Thought      bool   `json:"thought,omitempty"`
							FunctionCall *struct {
								ID   string         `json:"id"`
								Name string         `json:"name"`
								Args map[string]any `json:"args"`
							} `json:"functionCall,omitempty"`
						} `json:"parts"`
					} `json:"content"`
					FinishReason string `json:"finishReason,omitempty"`
				} `json:"candidates"`
				UsageMetadata struct {
					PromptTokenCount     int `json:"promptTokenCount"`
					CandidatesTokenCount int `json:"candidatesTokenCount"`
				} `json:"usageMetadata"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			continue
		}
		if len(env.Response.Candidates) == 0 {
			continue
		}
		cand := env.Response.Candidates[0]

		for _, p := range cand.Content.Parts {
			// Thought parts come first in a model turn (Gemini ordering). Emit
			// them as Anthropic thinking blocks so CC records reasoning content
			// and the SDK propagates it to all callers (jsonl, UI, etc.).
			if p.Thought {
				closeTextIfOpen()
				if p.Text == "" {
					// Header part with no content; defer block_start until we
					// see actual thought text (Anthropic SDK rejects empty
					// thinking blocks).
					continue
				}
				if !thinkingBlockStarted {
					geminiWriteSSE(w, "content_block_start", geminiBlockStart{
						Type:  "content_block_start",
						Index: blockIndex,
						ContentBlock: geminiAnthropicBlock{
							Type: "thinking", Thinking: "",
						},
					})
					thinkingBlockStarted = true
				}
				geminiWriteSSE(w, "content_block_delta", geminiBlockDelta{
					Type:  "content_block_delta",
					Index: blockIndex,
					Delta: map[string]any{"type": "thinking_delta", "thinking": p.Text},
				})
				if flusher != nil {
					flusher.Flush()
				}
				continue
			}
			if p.Text != "" {
				closeThinkingIfOpen()
				if !textBlockStarted {
					geminiWriteSSE(w, "content_block_start", geminiBlockStart{
						Type:         "content_block_start",
						Index:        blockIndex,
						ContentBlock: geminiAnthropicBlock{Type: "text", Text: ""},
					})
					textBlockStarted = true
				}
				geminiWriteSSE(w, "content_block_delta", geminiBlockDelta{
					Type:  "content_block_delta",
					Index: blockIndex,
					Delta: map[string]any{"type": "text_delta", "text": p.Text},
				})
				if flusher != nil {
					flusher.Flush()
				}
			}
			if p.FunctionCall != nil {
				closeTextIfOpen()
				closeThinkingIfOpen()
				hasToolUse = true
				fcID := p.FunctionCall.ID
				if fcID == "" {
					fcID = "toolu_" + geminiRandomID()
				} else if !strings.HasPrefix(fcID, "toolu_") {
					fcID = "toolu_" + fcID
				}
				argsJSON, _ := json.Marshal(p.FunctionCall.Args)
				if len(argsJSON) == 0 || string(argsJSON) == "null" {
					argsJSON = []byte("{}")
				}
				geminiWriteSSE(w, "content_block_start", geminiBlockStart{
					Type:  "content_block_start",
					Index: blockIndex,
					ContentBlock: geminiAnthropicBlock{
						Type:  "tool_use",
						ID:    fcID,
						Name:  p.FunctionCall.Name,
						Input: json.RawMessage("{}"),
					},
				})
				geminiWriteSSE(w, "content_block_delta", geminiBlockDelta{
					Type:  "content_block_delta",
					Index: blockIndex,
					Delta: map[string]any{
						"type":         "input_json_delta",
						"partial_json": string(argsJSON),
					},
				})
				geminiWriteSSE(w, "content_block_stop", geminiBlockStop{
					Type: "content_block_stop", Index: blockIndex,
				})
				blockIndex++
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if cand.FinishReason != "" {
			finalFinishReason = cand.FinishReason
		}
		if env.Response.UsageMetadata.PromptTokenCount > 0 {
			inputTokens = env.Response.UsageMetadata.PromptTokenCount
		}
		if env.Response.UsageMetadata.CandidatesTokenCount > 0 {
			outputTokens = env.Response.UsageMetadata.CandidatesTokenCount
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] gemini SSE scanner error: %v\n", appName, err)
	}

	closeThinkingIfOpen()
	closeTextIfOpen()

	stopReason := mapGeminiFinishReason(finalFinishReason, hasToolUse)
	// Diagnostic: capture every turn's finish state so we can see whether the
	// Gemini stream supplied finishReason and whether hasToolUse was flipped.
	// CC reports empty stop_reason in jsonl for most turns even though this
	// code path always yields non-empty — logging here confirms whether the
	// event is emitted with the right value, or if the upstream never sends
	// finishReason and hasToolUse tracking is stale.
	fmt.Fprintf(os.Stderr, "[%s] gemini stop_reason debug model=%s finishReason=%q hasToolUse=%v → stop_reason=%q\n",
		appName, areq.Model, finalFinishReason, hasToolUse, stopReason)
	msgDelta := geminiMsgDelta{
		Type: "message_delta",
		Usage: geminiAnthropicUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
		},
	}
	msgDelta.Delta.StopReason = stopReason
	geminiWriteSSE(w, "message_delta", msgDelta)
	geminiWriteSSE(w, "message_stop", geminiMsgStop{Type: "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}
