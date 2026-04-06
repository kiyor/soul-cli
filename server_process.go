package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
		args = append(args, "--append-system-prompt-file", opts.SystemPromptFile)
	}
	if opts.MCPConfig != "" {
		args = append(args, "--mcp-config", opts.MCPConfig)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}

	cmd := exec.Command(claudeBin, args...)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	} else {
		cmd.Dir = workspace
	}

	// Filter environment: remove CLAUDECODE to prevent nested detection
	env := filterEnv(os.Environ(), "CLAUDECODE")
	env = append(env, "CLAUDE_CODE_ENTRYPOINT=sdk-go")
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
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		done:   make(chan struct{}),
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

// sendMessage writes a user message to Claude Code's stdin.
func (p *claudeProcess) sendMessage(content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return fmt.Errorf("process already exited")
	default:
	}

	payload, _ := json.Marshal(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
		"session_id":         "default",
	})
	_, err := p.stdin.Write(append(payload, '\n'))
	return err
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
func (p *claudeProcess) alive() bool {
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

// randHex returns n random hex bytes as a string.
func randHex(n int) string {
	b := make([]byte, n)
	// Use PID + time as poor-man's random (no crypto needed for request IDs)
	v := uint64(time.Now().UnixNano()) ^ uint64(os.Getpid())
	for i := range b {
		b[i] = "0123456789abcdef"[v&0xf]
		v >>= 4
		if v == 0 {
			v = uint64(time.Now().UnixNano())
		}
	}
	return string(b)
}
