package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ── Stream-JSON message types (Claude Code ↔ weiran server) ──

// StreamMessage is the envelope for all stream-json messages (peek type/subtype first).
type StreamMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
}

// InitMessage is the first message from Claude Code after startup.
type InitMessage struct {
	Type           string      `json:"type"`
	Subtype        string      `json:"subtype"`
	CWD            string      `json:"cwd"`
	SessionID      string      `json:"session_id"`
	Tools          []string    `json:"tools"`
	MCPServers     []MCPServer `json:"mcp_servers"`
	Model          string      `json:"model"`
	PermissionMode string      `json:"permissionMode"`
}

// MCPServer describes a connected MCP server.
type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// AssistantMsg wraps a Claude assistant response.
type AssistantMsg struct {
	Type      string     `json:"type"`
	Message   APIMessage `json:"message"`
	SessionID string     `json:"session_id"`
	UUID      string     `json:"uuid,omitempty"`
}

// APIMessage is the inner message from Claude.
type APIMessage struct {
	Model      string         `json:"model"`
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	StopReason *string        `json:"stop_reason"`
	Usage      *TokenUsage    `json:"usage"`
}

// ContentBlock is a text/thinking/tool_use/tool_result block.
type ContentBlock struct {
	Type      string          `json:"type"` // text, thinking, tool_use, tool_result
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
}

// TokenUsage tracks token consumption.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ResultMessage signals the end of one conversation turn.
type ResultMessage struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMs   int     `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	StopReason   string  `json:"stop_reason"`
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// ── Claude Code process wrapper ──

// sessionOpts configures a new Claude Code subprocess.
type sessionOpts struct {
	WorkDir          string
	SystemPromptFile string
	MCPConfig        string
	Model            string
	MaxTurns         int
	Chrome           bool
	// ReplaceSoul — when true, pass --system-prompt-file instead of
	// --append-system-prompt-file. This strips CC's native system prompt
	// (intro, doing tasks, tone, output efficiency, session guidance,
	// auto-memory, MCP instructions, env info) and leaves only the soul
	// prompt. Server-side Anthropic safety blocks (injection defense,
	// privacy, copyright) are NOT affected — they're injected by the API,
	// not by CC. See conversation 2026-04-08 for source-code analysis.
	ReplaceSoul bool
	// ResumeID — Claude Code session ID to resume. When set, adds
	// --resume <id> so the new process inherits conversation history.
	// Used by setReplaceSoul to preserve context across mode toggle.
	ResumeID string
	// ServerSessionID — the server session ID, injected as
	// {APPNAME}_SESSION_ID env var for IPC CLI commands.
	ServerSessionID string
}

// claudeProcess wraps a running Claude Code subprocess with stream-json pipes.
type claudeProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	done     chan struct{}
	exitCode int
	mu       sync.Mutex // protects stdin writes

	// initReady is closed when the process emits its first "init" message,
	// signaling it's ready to accept user messages on stdin.
	initReady chan struct{}

	// suppressClose, when true, makes bridgeStdout skip the trailing "close"
	// SSE event on process exit. Used during intentional reload (e.g. chrome
	// toggle) so the UI doesn't see "Session ended."
	suppressClose atomic.Bool

	// Response waiters for synchronous control requests
	waitersMu sync.Mutex
	waiters   map[string]chan json.RawMessage
}

// spawnClaude starts a Claude Code subprocess in stream-json mode.
func spawnClaude(opts sessionOpts) (*claudeProcess, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	}

	if opts.SystemPromptFile != "" {
		if opts.ReplaceSoul {
			args = append(args, "--system-prompt-file", opts.SystemPromptFile)
		} else {
			args = append(args, "--append-system-prompt-file", opts.SystemPromptFile)
		}
	}
	if opts.MCPConfig != "" {
		args = append(args, "--mcp-config", opts.MCPConfig)
	}
	if opts.Model != "" {
		// For provider/model format (e.g. "zai/glm-5.1"), pass only the model name to --model
		args = append(args, "--model", providerModelName(opts.Model))
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	if opts.Chrome {
		args = append(args, "--chrome")
	}
	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	}

	cmd := exec.Command(claudeBin, args...)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	} else {
		cmd.Dir = workspace
	}

	// Filter environment: remove CLAUDECODE to prevent nested detection
	env := filterEnv(os.Environ(), "CLAUDECODE")
	env = append(env, "CLAUDE_CODE_ENTRYPOINT=sdk-cli")
	// Use model-aware env injection: provider models route directly, Anthropic models use proxy
	env = injectProxyEnvWithModel(env, opts.ServerSessionID, opts.Model)

	// IPC env vars: inject session ID, server URL, and auth token so child
	// processes can use `{cli} session send/read/search` CLI commands.
	// Env var prefix is derived from appName (e.g. "weiran" → "WEIRAN_").
	prefix := ipcEnvPrefix()
	if opts.ServerSessionID != "" {
		env = append(env, prefix+"_SESSION_ID="+opts.ServerSessionID)
	}
	if serverURL := os.Getenv(prefix + "_SERVER_URL"); serverURL != "" {
		env = append(env, prefix+"_SERVER_URL="+serverURL)
	} else if isServerMode {
		// Self-referencing: build URL from server config
		env = append(env, fmt.Sprintf(prefix+"_SERVER_URL=http://127.0.0.1:%d", serverPort))
	}
	if authToken := os.Getenv(prefix + "_AUTH_TOKEN"); authToken != "" {
		env = append(env, prefix+"_AUTH_TOKEN="+authToken)
	} else if serverAuthToken != "" {
		env = append(env, prefix+"_AUTH_TOKEN="+serverAuthToken)
	}

	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	proc := &claudeProcess{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		done:      make(chan struct{}),
		initReady: make(chan struct{}),
		waiters:   make(map[string]chan json.RawMessage),
	}

	// Monitor process exit in background
	go func() {
		err := cmd.Wait()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				proc.exitCode = exitErr.ExitCode()
			} else {
				proc.exitCode = 1
			}
		}
		close(proc.done)
	}()

	return proc, nil
}

// signalInit marks the process as initialized and ready to accept user messages.
// Called by bridgeStdout when the init message is received. Safe to call multiple times.
func (p *claudeProcess) signalInit() {
	select {
	case <-p.initReady:
		// already signaled
	default:
		close(p.initReady)
	}
}

// waitInit blocks until the process emits its init message or the timeout expires.
// Returns true if init was received, false on timeout or process exit.
func (p *claudeProcess) waitInit(timeout time.Duration) bool {
	select {
	case <-p.initReady:
		return true
	case <-p.done:
		return false
	case <-time.After(timeout):
		return false
	}
}

// sendMessage writes a user message to Claude Code's stdin.
// If the message contains ![alt](url) image patterns, extracts images and
// sends a content array with text + image blocks (base64 encoded).
func (p *claudeProcess) sendMessage(content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return fmt.Errorf("process already exited")
	default:
	}

	// Build content: detect image markdown and convert to content blocks
	msgContent := buildMessageContent(content)

	payload, _ := json.Marshal(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": msgContent},
		"parent_tool_use_id": nil,
		"session_id":         "default",
	})
	_, err := p.stdin.Write(append(payload, '\n'))
	return err
}

// buildMessageContent parses the message for image markdown and returns
// either a plain string (no images) or a content array (text + image blocks).
func buildMessageContent(content string) any {
	re := regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	matches := re.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return content // no images, keep as plain string
	}

	var blocks []map[string]any
	lastIdx := 0

	for _, match := range matches {
		// Text before this image
		if match[0] > lastIdx {
			text := strings.TrimSpace(content[lastIdx:match[0]])
			if text != "" {
				blocks = append(blocks, map[string]any{
					"type": "text",
					"text": text,
				})
			}
		}

		// Image URL
		imgURL := content[match[4]:match[5]]
		altText := content[match[2]:match[3]]

		imgBlock := resolveImageBlock(imgURL, altText)
		if imgBlock != nil {
			blocks = append(blocks, imgBlock)
		}

		lastIdx = match[1]
	}

	// Trailing text
	if lastIdx < len(content) {
		text := strings.TrimSpace(content[lastIdx:])
		if text != "" {
			blocks = append(blocks, map[string]any{
				"type": "text",
				"text": text,
			})
		}
	}

	if len(blocks) == 0 {
		return content
	}
	return blocks
}

// resolveImageBlock reads an image (local file or HTTP URL) and returns
// a Claude API image content block with base64 data, or nil on failure.
func resolveImageBlock(imgURL string, altText string) map[string]any {
	var data []byte
	var mediaType string
	var err error

	if strings.HasPrefix(imgURL, "/uploads/") {
		// Local file: resolve against workspace uploads dir
		filename := filepath.Base(imgURL)
		localPath := filepath.Join(workspace, "uploads", filename)
		data, err = os.ReadFile(localPath)
		mediaType = guessMediaType(filename)
	} else if strings.HasPrefix(imgURL, "http://") || strings.HasPrefix(imgURL, "https://") {
		// Remote URL: download with timeout
		client := &http.Client{Timeout: 15 * time.Second}
		resp, respErr := client.Get(imgURL)
		if respErr != nil {
			fmt.Fprintf(os.Stderr, "[%s] image download failed: %v\n", appName, respErr)
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "[%s] image download status: %d\n", appName, resp.StatusCode)
			return nil
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB max
		mediaType = resp.Header.Get("Content-Type")
		if mediaType == "" {
			mediaType = guessMediaType(imgURL)
		}
	} else {
		return nil // unsupported URL scheme
	}

	if err != nil || len(data) == 0 {
		fmt.Fprintf(os.Stderr, "[%s] image read failed: %v\n", appName, err)
		return nil
	}

	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       base64.StdEncoding.EncodeToString(data),
		},
	}
}

// guessMediaType returns a MIME type based on file extension.
func guessMediaType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	default:
		return "image/png"
	}
}

// controlRequest sends a control request (interrupt, set_model, context_usage, etc.).
func (p *claudeProcess) controlRequest(subtype string, extra map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return fmt.Errorf("process already exited")
	default:
	}

	req := map[string]any{"subtype": subtype}
	for k, v := range extra {
		req[k] = v
	}
	payload, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("req_%d_%s", time.Now().UnixMilli(), randHex(4)),
		"request":    req,
	})
	_, err := p.stdin.Write(append(payload, '\n'))
	return err
}

// controlRequestSync sends a control request and waits for the matching response.
func (p *claudeProcess) controlRequestSync(subtype string, extra map[string]any, timeout time.Duration) (json.RawMessage, error) {
	reqID := fmt.Sprintf("req_%d_%s", time.Now().UnixMilli(), randHex(4))

	// Register waiter before sending
	ch := make(chan json.RawMessage, 1)
	p.waitersMu.Lock()
	p.waiters[reqID] = ch
	p.waitersMu.Unlock()

	defer func() {
		p.waitersMu.Lock()
		delete(p.waiters, reqID)
		p.waitersMu.Unlock()
	}()

	// Send request
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()
		return nil, fmt.Errorf("process already exited")
	default:
	}
	req := map[string]any{"subtype": subtype}
	for k, v := range extra {
		req[k] = v
	}
	payload, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": reqID,
		"request":    req,
	})
	_, err := p.stdin.Write(append(payload, '\n'))
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}

	// Wait for response
	select {
	case resp := <-ch:
		return resp, nil
	case <-p.done:
		return nil, fmt.Errorf("process exited while waiting")
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for response")
	}
}

// deliverResponse routes a control_response to the matching waiter (if any).
// Claude Code format: {"type":"control_response","response":{"subtype":"success","request_id":"req_xxx","response":{...}}}
// Returns true if a waiter consumed it.
func (p *claudeProcess) deliverResponse(raw json.RawMessage) bool {
	var peek struct {
		Response struct {
			RequestID string `json:"request_id"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &peek) != nil || peek.Response.RequestID == "" {
		return false
	}
	p.waitersMu.Lock()
	ch, ok := p.waiters[peek.Response.RequestID]
	p.waitersMu.Unlock()
	if ok {
		select {
		case ch <- raw:
		default:
		}
		return true
	}
	return false
}

// shutdown gracefully stops the Claude Code process.
// Close stdin → wait 5s → SIGTERM → wait 5s → SIGKILL.
func (p *claudeProcess) shutdown() {
	p.mu.Lock()
	p.stdin.Close()
	p.mu.Unlock()

	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
		p.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
	}
	<-p.done
}

// alive returns true if the process hasn't exited yet.
// Safe to call on nil receiver (returns false).
func (p *claudeProcess) alive() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// readLines reads stdout line by line, parses JSON, and calls handler for each valid message.
// Non-JSON lines (e.g. [SandboxDebug]) are silently skipped.
// Blocks until stdout is closed (process exits or stdin closed).
func (p *claudeProcess) readLines(handler func(msgType string, raw json.RawMessage)) {
	scanner := bufio.NewScanner(p.stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Skip non-JSON lines
		if line[0] != '{' {
			continue
		}

		// Peek at type
		var peek StreamMessage
		if err := json.Unmarshal(line, &peek); err != nil {
			continue
		}

		// Make a copy for the handler (scanner reuses buffer)
		raw := make(json.RawMessage, len(line))
		copy(raw, line)

		handler(peek.Type, raw)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] readLines scanner error: %v\n", appName, err)
	}
}

// ── Helpers ──

// filterEnv returns env slice with entries matching prefix removed.
func filterEnv(env []string, prefix string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix+"=") && !strings.HasPrefix(e, prefix+"_") {
			out = append(out, e)
		}
	}
	return out
}

// randHex returns n random hex characters as a string.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:n]
}
