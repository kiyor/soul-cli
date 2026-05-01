// webhook.go — Skill-driven webhook framework.
//
// Concept: a single POST /webhook endpoint receives JSON bodies (typically
// from iPhone Shortcuts share-sheets). The server routes by `action` field
// to a plugin file under workspace/webhook/. Plugins are arbitrary
// executables (.sh / .py / .go / .js / .ts / chmod+x binaries). The plugin
// receives the raw JSON on stdin, prints a result JSON on stdout, exits 0
// on success.
//
// On failure (exit != 0, exit == 64 explicit fallback signal, timeout, or
// missing plugin), the framework spawns an AI fallback session that's
// instructed to (1) complete the task by any means and (2) repair the
// failing plugin. This makes the webhook framework self-healing — the
// AI's fix lands in workspace/webhook/, ready to run next time.
//
// Plugin discovery: workspace/webhook/<action>.<ext>. The basename equals
// the action; extension picks the interpreter. Files starting with `_`
// or `.` are ignored (so `_bin/`, `_router.yaml` etc. are out-of-band).
//
// Security: action names are validated to a strict charset, the plugin
// directory is fixed (no traversal), PATH is reset before exec, stdout
// is capped at 10MB. The endpoint reuses the main weiran auth token via
// authMiddleware; webhook callers (e.g. iPhone Shortcuts) must include
// `Authorization: Bearer <token>`.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ── Constants ──────────────────────────────────────────────────────────────

const (
	webhookDirName             = "webhook"        // workspace subdir for plugins
	webhookBinSubdir           = "_bin"           // compiled .go plugin cache
	webhookDefaultTimeout      = 30 * time.Second // override via body.timeout
	webhookMaxTimeout          = 5 * time.Minute  // hard cap
	webhookMaxStdoutBytes      = 10 * 1024 * 1024 // 10MB
	webhookMaxBodyBytes        = 1 << 20          // 1MB inbound
	webhookFallbackExitCode    = 64               // plugin signals "I don't handle this"
	webhookDefaultDownloadRoot = "/Volumes/weiran/share/downloads"
	webhookAuditFieldMax       = 8 * 1024 // truncate stdout/stderr for audit row
)

// errPluginNotFound: plugin file does not exist for the requested action.
// Triggers AI fallback rather than a hard error.
var errPluginNotFound = errors.New("plugin not found")

// hostAliases collapses URL hosts into canonical short names that map 1:1
// to plugin filenames and download subdirectories. Anything not listed
// keeps its bare hostname (with `www.` stripped).
var hostAliases = map[string]string{
	"x.com":               "x",
	"twitter.com":         "x",
	"mobile.twitter.com":  "x",
	"fxtwitter.com":       "x",
	"vxtwitter.com":       "x",
	"youtube.com":         "youtube",
	"youtu.be":            "youtube",
	"m.youtube.com":       "youtube",
	"music.youtube.com":   "youtube",
	"reddit.com":          "reddit",
	"old.reddit.com":      "reddit",
	"redd.it":             "reddit",
	"i.redd.it":           "reddit",
	"v.redd.it":           "reddit",
	"redgifs.com":         "redgifs",
	"www.redgifs.com":     "redgifs",
	"github.com":          "github",
	"www.github.com":      "github",
}

// downloadSubdirSeed: subdirs auto-mkdir'd on server start. Adding a new
// host doesn't require rebooting — plugins can mkdir their own paths.
var downloadSubdirSeed = []string{"x", "youtube", "reddit", "redgifs", "_misc"}

// ── Path helpers ───────────────────────────────────────────────────────────

func webhookDir() string {
	return filepath.Join(workspace, webhookDirName)
}

func webhookBinDir() string {
	return filepath.Join(webhookDir(), webhookBinSubdir)
}

func webhookDownloadRoot() string {
	if v := strings.TrimSpace(os.Getenv("WEIRAN_DOWNLOAD_ROOT")); v != "" {
		return v
	}
	return webhookDefaultDownloadRoot
}

// initWebhookDirs is called once at server start. Idempotent: existing
// dirs are left alone, missing ones get created with safe perms.
//
// Returns nil on best-effort success — failures are logged but never
// fatal because the webhook subsystem is optional. The download root is
// on an external SSD; if that volume isn't mounted we still want the
// rest of the server to come up.
func initWebhookDirs() {
	if err := os.MkdirAll(webhookBinDir(), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] webhook: mkdir _bin failed: %v\n", appName, err)
	}
	root := webhookDownloadRoot()
	for _, sub := range downloadSubdirSeed {
		// 0755 here (not 0700) because download root is shared with you
		// over SMB/WebDAV — group/other read needed for listing.
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			// Don't spam the log if the volume isn't mounted; one warning is enough.
			if _, statErr := os.Stat(root); os.IsNotExist(statErr) {
				fmt.Fprintf(os.Stderr, "[%s] webhook: download root %s not mounted; plugins must create their own dirs\n", appName, root)
				return
			}
			fmt.Fprintf(os.Stderr, "[%s] webhook: mkdir %s/%s failed: %v\n", appName, root, sub, err)
		}
	}
}

// ── Host normalization ─────────────────────────────────────────────────────

// normalizeHost extracts a canonical short host name from a URL. Returns
// "" when the input isn't a parseable URL with a host. The result is also
// used as the implicit action when the request body has no `action` field.
func normalizeHost(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	// Parse forgivingly: many share-sheet payloads omit scheme.
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	host = strings.TrimPrefix(host, "www.")
	if alias, ok := hostAliases[host]; ok {
		return alias
	}
	return host
}

// ── Plugin discovery ───────────────────────────────────────────────────────

type pluginInfo struct {
	Path   string // absolute path to plugin file
	Action string // basename without extension
	Ext    string // ".py" / ".go" / ".sh" / ... or "" for no extension
}

// validAction enforces a strict charset for action names. This is what
// stops `{"action":"../../../etc/passwd"}` cold — the action *is* the
// filename basename, so anything outside [a-zA-Z0-9_-] is rejected
// before we ever touch the filesystem.
func validAction(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// resolvePlugin locates the plugin file matching `action`. Returns
// errPluginNotFound when no candidate exists, or a wrapped error when
// multiple files match the same basename (config error — we refuse to
// silently pick one).
func resolvePlugin(action string) (*pluginInfo, error) {
	if !validAction(action) {
		return nil, fmt.Errorf("invalid action %q", action)
	}
	dir := webhookDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errPluginNotFound
		}
		return nil, err
	}

	var matches []*pluginInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip housekeeping files (`_bin/` is a dir but we double-guard;
		// `_router.yaml`, `.gitignore`, etc. are out-of-band).
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		if base != action {
			continue
		}
		switch ext {
		case "", ".sh", ".py", ".go", ".js", ".ts":
			matches = append(matches, &pluginInfo{
				Path:   filepath.Join(dir, name),
				Action: action,
				Ext:    ext,
			})
		default:
			// Unknown extension: ignore (could be docs, README, etc.)
		}
	}
	if len(matches) == 0 {
		return nil, errPluginNotFound
	}
	if len(matches) > 1 {
		var paths []string
		for _, m := range matches {
			paths = append(paths, filepath.Base(m.Path))
		}
		return nil, fmt.Errorf("multiple plugins for action %q: %s (rename or delete extras)",
			action, strings.Join(paths, ", "))
	}
	return matches[0], nil
}

// ── Plugin execution ───────────────────────────────────────────────────────

// pluginCommand returns the exec.Cmd that runs the plugin, picking an
// interpreter by extension or honoring shebang+chmod+x. The caller
// re-wraps with exec.CommandContext for timeout enforcement.
func pluginCommand(p *pluginInfo) (*exec.Cmd, error) {
	// .go: build-and-cache to _bin/<action>, then exec the binary.
	if p.Ext == ".go" {
		bin, err := compileGoPlugin(p)
		if err != nil {
			return nil, err
		}
		return exec.Command(bin), nil
	}

	info, err := os.Stat(p.Path)
	if err != nil {
		return nil, err
	}
	executable := info.Mode()&0o111 != 0

	// Honor shebang when the file is chmod+x — most flexible path: lets
	// the plugin author pick its own interpreter (`#!/usr/bin/env bun`).
	if executable {
		first, _ := readFirstLine(p.Path)
		if strings.HasPrefix(first, "#!") {
			return exec.Command(p.Path), nil
		}
	}

	switch p.Ext {
	case ".py":
		return exec.Command("python3", p.Path), nil
	case ".sh":
		return exec.Command("bash", p.Path), nil
	case ".js":
		return exec.Command("node", p.Path), nil
	case ".ts":
		// bun run handles TS natively; falls back to node+ts-node only if user prefers.
		return exec.Command("bun", "run", p.Path), nil
	case "":
		// Bare binary — must be chmod+x.
		if !executable {
			return nil, fmt.Errorf("plugin %s has no extension and is not executable", p.Path)
		}
		return exec.Command(p.Path), nil
	}
	return nil, fmt.Errorf("unsupported plugin extension: %s", p.Ext)
}

// readFirstLine reads up to 256 bytes from the file and returns whatever
// precedes the first '\n'. Used only for shebang detection.
func readFirstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	line := string(buf[:n])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.TrimRight(line, "\r"), nil
}

// compileGoPlugin builds <action>.go to webhook/_bin/<action>, with a
// crude mtime cache: rebuild only when the source is newer than the
// binary. This keeps Go plugins fast (sub-millisecond exec after the
// first build) without external tooling.
//
// On build failure we return the combined go-build output so the
// fallback session sees what's wrong.
func compileGoPlugin(p *pluginInfo) (string, error) {
	if err := os.MkdirAll(webhookBinDir(), 0o700); err != nil {
		return "", err
	}
	binPath := filepath.Join(webhookBinDir(), p.Action)

	srcStat, err := os.Stat(p.Path)
	if err != nil {
		return "", err
	}
	if binStat, err := os.Stat(binPath); err == nil && binStat.ModTime().After(srcStat.ModTime()) {
		return binPath, nil
	}

	cmd := exec.Command("go", "build", "-o", binPath, p.Path)
	cmd.Dir = webhookDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build failed: %v\n%s", err, string(out))
	}
	return binPath, nil
}

// webhookResult bundles the outcome of running one plugin. Used by both
// the HTTP handler (to decide success vs fallback) and the audit logger.
type webhookResult struct {
	PluginPath string
	ExitCode   int
	Duration   time.Duration
	Stdout     []byte
	Stderr     []byte
	TimedOut   bool

	// ParsedJSON is populated when stdout is valid JSON. Plugins should
	// emit `{"status":"ok|error",...}` per the documented protocol.
	ParsedJSON map[string]any
	Status     string
}

// runPlugin executes the plugin with `body` on stdin, capturing stdout/
// stderr (each capped to webhookMaxStdoutBytes), with a deadline of
// `timeout`. Errors here represent setup failures (couldn't fork etc.);
// process-level failures are recorded on the result and returned with
// nil error — callers branch on res.ExitCode/TimedOut.
func runPlugin(parent context.Context, p *pluginInfo, body []byte, env map[string]string, timeout time.Duration) (*webhookResult, error) {
	tmpl, err := pluginCommand(p)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// Re-build via CommandContext so timeout actually kills the child.
	args := tmpl.Args
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = webhookDir()
	cmd.Env = buildPluginEnv(env)
	cmd.Stdin = bytes.NewReader(body)

	stdout := &cappedBuffer{cap: webhookMaxStdoutBytes}
	stderr := &cappedBuffer{cap: webhookMaxStdoutBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	res := &webhookResult{
		PluginPath: p.Path,
		Duration:   dur,
		Stdout:     bytes.TrimRight(stdout.Bytes(), "\n"),
		Stderr:     stderr.Bytes(),
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
	} else if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			// e.g. binary missing, fork failure
			return nil, runErr
		}
	}

	// Best-effort JSON parse. Plugins are *expected* to emit JSON but
	// we don't fail the whole call if they didn't — we just won't have
	// structured fields to forward to the client.
	if len(res.Stdout) > 0 {
		var parsed map[string]any
		if json.Unmarshal(res.Stdout, &parsed) == nil {
			res.ParsedJSON = parsed
			if s, ok := parsed["status"].(string); ok {
				res.Status = s
			}
		}
	}
	return res, nil
}

// cappedBuffer is an io.Writer with a hard byte limit. Used to bound
// stdout/stderr capture per-call — a runaway plugin can't OOM the server.
type cappedBuffer struct {
	bytes.Buffer
	cap     int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.cap - c.Buffer.Len()
	if remaining <= 0 {
		c.dropped += len(p)
		return len(p), nil // pretend we wrote so the child doesn't block
	}
	if len(p) > remaining {
		c.Buffer.Write(p[:remaining])
		c.dropped += len(p) - remaining
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

// buildPluginEnv constructs the env slice for the plugin process. We
// reset PATH to a known-safe minimum (homebrew + system) so plugins
// don't inherit weird local state, and surface only the env vars the
// protocol documents.
func buildPluginEnv(extras map[string]string) []string {
	base := []string{
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=" + os.Getenv("HOME"),
		"LANG=" + ifEmpty(os.Getenv("LANG"), "en_US.UTF-8"),
		"TZ=" + os.Getenv("TZ"),
	}
	for k, v := range extras {
		base = append(base, k+"="+v)
	}
	return base
}

// ── Audit log ──────────────────────────────────────────────────────────────

type webhookAuditRow struct {
	Timestamp         string
	RequestID         string
	Action            string
	Host              string
	PluginPath        string
	ExitCode          int
	DurationMs        int64
	FallbackSessionID string
	BodyJSON          string
	Stdout            string
	StderrTail        string
	TimedOut          bool
	Status            string // ok | error | fallback
	RemoteAddr        string
}

func recordWebhookAudit(audit webhookAuditRow) {
	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] webhook: openDB for audit failed: %v\n", appName, err)
		return
	}
	defer db.Close()

	_, err = db.Exec(`INSERT INTO webhook_audit (
		timestamp, request_id, action, host, plugin_path, exit_code,
		duration_ms, fallback_session_id, body_json, stdout, stderr_tail,
		timed_out, status, remote_addr
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		audit.Timestamp, audit.RequestID, audit.Action, audit.Host, audit.PluginPath,
		audit.ExitCode, audit.DurationMs, audit.FallbackSessionID, audit.BodyJSON,
		audit.Stdout, audit.StderrTail, audit.TimedOut, audit.Status, audit.RemoteAddr,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] webhook: audit insert failed: %v\n", appName, err)
	}
}

// ── HTTP handler ───────────────────────────────────────────────────────────

// handleWebhook is the body of POST /webhook. It does:
//
//  1. parse + validate the JSON body
//  2. extract action (or infer from URL host)
//  3. resolve the plugin file
//  4. run it with stdin/stdout JSON protocol
//  5. on success: 200 + plugin's parsed result
//  6. on miss/fail/timeout: 202 + spawn AI fallback session
//
// Auth is enforced at the mux layer via authMiddleware(cfg.Token, ...);
// no separate token logic lives here.
func handleWebhook(sm *sessionManager, w http.ResponseWriter, r *http.Request) {
	bodyReader := io.LimitReader(r.Body, webhookMaxBodyBytes)
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty body"})
		return
	}

	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	action, _ := top["action"].(string)
	rawURL, _ := top["url"].(string)
	host := normalizeHost(rawURL)

	// If body has no explicit action, fall back to host name. This is
	// what makes the iPhone share-sheet experience seamless: a Shortcut
	// can POST `{"url":"https://x.com/..."}` with no `action` field and
	// still hit the `x` plugin.
	if action == "" && host != "" {
		action = host
	}

	requestID := newRequestID()
	timestamp := time.Now().UTC().Format(time.RFC3339)

	audit := webhookAuditRow{
		Timestamp:  timestamp,
		RequestID:  requestID,
		Action:     action,
		Host:       host,
		BodyJSON:   truncateString(string(body), webhookAuditFieldMax),
		RemoteAddr: r.RemoteAddr,
	}

	fmt.Fprintf(os.Stderr, "[%s] webhook: %s action=%q host=%q\n", appName, requestID, action, host)

	// ── Resolve plugin ──
	var p *pluginInfo
	var lookupErr error
	if action != "" {
		p, lookupErr = resolvePlugin(action)
	} else {
		lookupErr = errPluginNotFound
	}

	if lookupErr != nil && !errors.Is(lookupErr, errPluginNotFound) {
		// Hard config error (multi-match, invalid action). Don't fallback —
		// surface it so the caller knows their setup is broken.
		audit.Status = "error"
		audit.StderrTail = lookupErr.Error()
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"request_id": requestID,
			"error":      lookupErr.Error(),
		})
		return
	}

	if errors.Is(lookupErr, errPluginNotFound) {
		// Plugin missing → spawn AI fallback session.
		sessionID := triggerFallback(sm, fallbackContext{
			RequestID:  requestID,
			Action:     action,
			Host:       host,
			Body:       body,
			Reason:     "plugin not found",
			ExitCode:   0,
			Stderr:     "",
			TimedOut:   false,
			PluginPath: filepath.Join(webhookDir(), action+".(none)"),
		})
		audit.Status = "fallback"
		audit.FallbackSessionID = sessionID
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": requestID,
			"status":     "fallback",
			"reason":     "plugin not found",
			"session_id": sessionID,
		})
		return
	}

	audit.PluginPath = p.Path

	// ── Run plugin ──
	timeout := webhookDefaultTimeout
	if v, ok := top["timeout"].(float64); ok && v > 0 {
		req := time.Duration(v) * time.Second
		if req > webhookMaxTimeout {
			req = webhookMaxTimeout
		}
		timeout = req
	}

	env := pluginEnvFor(requestID, action, host)
	res, runErr := runPlugin(r.Context(), p, body, env, timeout)
	if runErr != nil {
		audit.Status = "error"
		audit.StderrTail = runErr.Error()
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"request_id": requestID,
			"error":      runErr.Error(),
		})
		return
	}

	audit.ExitCode = res.ExitCode
	audit.DurationMs = res.Duration.Milliseconds()
	audit.Stdout = truncateString(string(res.Stdout), webhookAuditFieldMax)
	audit.StderrTail = truncateString(string(res.Stderr), webhookAuditFieldMax)
	audit.TimedOut = res.TimedOut

	// ── Decide success vs fallback ──
	switch {
	case res.TimedOut:
		sessionID := triggerFallback(sm, fallbackContext{
			RequestID:  requestID,
			Action:     action,
			Host:       host,
			Body:       body,
			Reason:     fmt.Sprintf("plugin timed out after %s", timeout),
			ExitCode:   res.ExitCode,
			Stderr:     string(res.Stderr),
			TimedOut:   true,
			PluginPath: p.Path,
		})
		audit.Status = "fallback"
		audit.FallbackSessionID = sessionID
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": requestID,
			"status":     "fallback",
			"reason":     "timeout",
			"session_id": sessionID,
		})

	case res.ExitCode == webhookFallbackExitCode:
		sessionID := triggerFallback(sm, fallbackContext{
			RequestID:  requestID,
			Action:     action,
			Host:       host,
			Body:       body,
			Reason:     "plugin signaled fallback (exit 64)",
			ExitCode:   res.ExitCode,
			Stderr:     string(res.Stderr),
			PluginPath: p.Path,
		})
		audit.Status = "fallback"
		audit.FallbackSessionID = sessionID
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": requestID,
			"status":     "fallback",
			"reason":     "plugin requested fallback",
			"session_id": sessionID,
		})

	case res.ExitCode != 0:
		sessionID := triggerFallback(sm, fallbackContext{
			RequestID:  requestID,
			Action:     action,
			Host:       host,
			Body:       body,
			Reason:     fmt.Sprintf("plugin exited %d", res.ExitCode),
			ExitCode:   res.ExitCode,
			Stderr:     string(res.Stderr),
			PluginPath: p.Path,
		})
		audit.Status = "fallback"
		audit.FallbackSessionID = sessionID
		recordWebhookAudit(audit)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"request_id": requestID,
			"status":     "fallback",
			"reason":     fmt.Sprintf("exit %d", res.ExitCode),
			"session_id": sessionID,
		})

	default:
		// Success.
		audit.Status = ifEmpty(res.Status, "ok")
		recordWebhookAudit(audit)
		if res.ParsedJSON != nil {
			res.ParsedJSON["request_id"] = requestID
			writeJSON(w, http.StatusOK, res.ParsedJSON)
		} else {
			writeJSON(w, http.StatusOK, map[string]any{
				"request_id": requestID,
				"status":     "ok",
				"stdout":     string(res.Stdout),
			})
		}
	}
}

// pluginEnvFor builds the env-var map injected into plugin processes.
// Documented contract — adding a new key here is non-breaking, removing
// or renaming would be.
func pluginEnvFor(requestID, action, host string) map[string]string {
	hostKey := host
	if hostKey == "" {
		hostKey = "_misc"
	}
	root := webhookDownloadRoot()
	serverURL := fmt.Sprintf("http://127.0.0.1:%d", serverPort)
	return map[string]string{
		"WEIRAN_REQUEST_ID":    requestID,
		"WEIRAN_HOOK_ACTION":   action,
		"WEIRAN_HOST":          host,
		"WEIRAN_DOWNLOAD_ROOT": root,
		"WEIRAN_OUTPUT_DIR":    filepath.Join(root, hostKey),
		"WEIRAN_SERVER_URL":    serverURL,
		"WEIRAN_AUTH_TOKEN":    serverAuthToken,
	}
}

// ── Fallback session ───────────────────────────────────────────────────────

type fallbackContext struct {
	RequestID  string
	Action     string
	Host       string
	Body       []byte
	Reason     string
	ExitCode   int
	Stderr     string
	TimedOut   bool
	PluginPath string
}

// triggerFallback spawns an AI fallback session and returns its ID
// (empty on spawn failure, in which case the caller should still
// audit-log the attempt). The session is given a directive in
// InitialMessage that tells it to (a) complete the task by any means
// and (b) repair the plugin in-place, then self-destruct.
func triggerFallback(sm *sessionManager, fc fallbackContext) string {
	sessionName := fmt.Sprintf("webhook-fallback-%s-%s",
		ifEmpty(fc.Action, "unknown"),
		time.Now().Format("0102-1504"))

	tags := []string{"webhook", "fallback"}
	if fc.Action != "" {
		tags = append(tags, "action:"+fc.Action)
	}
	if fc.Host != "" {
		tags = append(tags, "host:"+fc.Host)
	}

	initialMsg := buildFallbackMessage(fc)

	sess, err := sm.createSessionWithOpts(sessionCreateOpts{
		Name:           sessionName,
		Project:        workspace,
		Soul:           true,
		Category:       "webhook-fallback",
		Tags:           tags,
		InitialMessage: initialMsg,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] webhook: fallback spawn failed: %v\n", appName, err)
		return ""
	}

	// Capture FirstMsg synchronously so the snapshot we exposed to the
	// caller via session list reflects the fallback context immediately.
	cleanInitial, _ := sanitizeSoulPatchInput(initialMsg)
	sess.mu.Lock()
	if sess.FirstMsg == "" {
		sess.FirstMsg = cleanInitial
	}
	sess.mu.Unlock()

	// Send initial message after Claude finishes init handshake. This
	// mirrors the pattern in POST /api/sessions (server.go:869) — the
	// 30s timeout absorbs slow proxy spinups.
	go func() {
		if !sess.process.waitInit(30 * time.Second) {
			fmt.Fprintf(os.Stderr, "[%s] webhook: fallback %s init timeout, sending anyway\n",
				appName, shortID(sess.ID))
		}
		injection := sess.prepareSoulPatch(cleanInitial)
		userEvent, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": injection.DisplayMessage},
		})
		sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})
		if err := sess.process.sendMessage(injection.Outbound); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] webhook: fallback %s sendMessage failed: %v\n",
				appName, shortID(sess.ID), err)
			return
		}
		sess.commitSoulPatchInjection(injection)
	}()

	return sess.ID
}

// buildFallbackMessage assembles the directive that becomes the fallback
// session's first user message. Everything in here is interpreted by
// you (未然) the AI in that fresh session — it never round-trips through
// templating outside of fmt.Sprintf.
func buildFallbackMessage(fc fallbackContext) string {
	timedOutStr := "no"
	if fc.TimedOut {
		timedOutStr = "yes"
	}

	stderrTail := fc.Stderr
	if len(stderrTail) > 4000 {
		stderrTail = "...(truncated)\n" + stderrTail[len(stderrTail)-4000:]
	}
	if stderrTail == "" {
		stderrTail = "(empty)"
	}

	pluginPath := fc.PluginPath
	if pluginPath == "" {
		pluginPath = "(unknown)"
	}

	hostKey := fc.Host
	if hostKey == "" {
		hostKey = "_misc"
	}

	return fmt.Sprintf(`你以 webhook fallback 模式被唤醒。

## 失败的 action

- action: `+"`"+`%s`+"`"+`
- host: `+"`"+`%s`+"`"+`
- plugin: `+"`"+`%s`+"`"+`

## 请求体

`+"```json\n%s\n```"+`

## 失败信息

- request_id: %s
- exit_code: %d
- timed_out: %s
- reason: %s

stderr 尾部:

`+"```\n%s\n```"+`

## 你要做两件事

**1. 完成本次任务**（用任何方法把 download / 归档 / 处理 干完）

- 输出根: `+"`"+`$WEIRAN_DOWNLOAD_ROOT/%s/`+"`"+`（也就是 `+"`"+`/Volumes/weiran/share/downloads/%s/`+"`"+`）
- 完成后用 `+"`"+`weiran notify`+"`"+` 通知主人结果（一句话即可）

**2. 修复 plugin** `+"`"+`%s`+"`"+`

- 读源码 → 定位 bug → 改 → 本地 dry-run 一次（`+"`"+`echo '<json>' | python3 plugin.py`+"`"+` 之类）
- 修不动就在文件顶部加 `+"`"+`# FIXME: <症状>`+"`"+` 标记，不要让脚本悄悄留 bug
- plugin 不存在就新建一个，参考 `+"`"+`workspace/webhook/`+"`"+` 下其他文件，遵循 stdin/stdout JSON 协议

完成后用 `+"`"+`weiran session close $WEIRAN_SESSION_ID`+"`"+` 自我销毁。
不写报告，不等确认，只做事 + notify。`,
		fc.Action,
		fc.Host,
		pluginPath,
		prettyJSON(fc.Body),
		fc.RequestID,
		fc.ExitCode,
		timedOutStr,
		fc.Reason,
		stderrTail,
		hostKey,
		hostKey,
		pluginPath,
	)
}

// ── Webhook tail (CLI) ─────────────────────────────────────────────────────

// runWebhookTail backs `weiran webhook tail`. Prints the last N rows of
// webhook_audit in reverse-chronological order. Surfaces fallback session
// IDs so you can `weiran session read <id>` to see what the AI did.
func runWebhookTail(args []string) {
	limit := 20
	verbose := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n", "--limit":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &limit)
				i++
			}
		case "-v", "--verbose":
			verbose = true
		}
	}
	if limit <= 0 {
		limit = 20
	}

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "openDB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT timestamp, request_id, action, host, plugin_path,
		exit_code, duration_ms, fallback_session_id, status, timed_out, stderr_tail
		FROM webhook_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	type row struct {
		ts, rid, action, host, plugin, status, stderrTail, fallbackID string
		exitCode                                                      int
		durMs                                                         int64
		timedOut                                                      bool
	}
	var collected []row
	for rows.Next() {
		var r row
		var to int
		if err := rows.Scan(&r.ts, &r.rid, &r.action, &r.host, &r.plugin,
			&r.exitCode, &r.durMs, &r.fallbackID, &r.status, &to, &r.stderrTail); err != nil {
			continue
		}
		r.timedOut = to != 0
		collected = append(collected, r)
	}

	if len(collected) == 0 {
		fmt.Println("(no webhook audit rows yet)")
		return
	}

	for i := len(collected) - 1; i >= 0; i-- { // chrono order
		r := collected[i]
		statusTag := r.status
		if r.timedOut {
			statusTag += "/TIMEOUT"
		}
		fmt.Printf("%s  %s  action=%s  host=%s  exit=%d  %dms  status=%s",
			r.ts, r.rid, ifEmpty(r.action, "-"), ifEmpty(r.host, "-"),
			r.exitCode, r.durMs, statusTag)
		if r.fallbackID != "" {
			fmt.Printf("  fallback=%s", shortID(r.fallbackID))
		}
		fmt.Println()
		if verbose && r.stderrTail != "" {
			fmt.Printf("    plugin: %s\n", r.plugin)
			for _, line := range strings.Split(strings.TrimRight(r.stderrTail, "\n"), "\n") {
				fmt.Printf("    | %s\n", line)
			}
		}
	}
}

// runWebhookCmd dispatches `weiran webhook <sub>`. Currently only `tail`
// is implemented; `list-plugins` and `test` are TODO for v2.
func runWebhookCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: weiran webhook <tail|list>")
		fmt.Println("  tail [-n N] [-v]   show recent webhook invocations")
		fmt.Println("  list               list discovered plugins")
		os.Exit(1)
	}
	switch args[0] {
	case "tail":
		runWebhookTail(args[1:])
	case "list":
		runWebhookList()
	default:
		fmt.Fprintf(os.Stderr, "unknown webhook subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

// runWebhookList prints what's discoverable in workspace/webhook/.
func runWebhookList() {
	dir := webhookDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("(no webhook directory at %s — create it and drop plugins in)\n", dir)
			return
		}
		fmt.Fprintf(os.Stderr, "read %s: %v\n", dir, err)
		os.Exit(1)
	}
	any := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		switch ext {
		case "", ".sh", ".py", ".go", ".js", ".ts":
			info, _ := e.Info()
			perm := ""
			if info != nil && info.Mode()&0o111 != 0 {
				perm = " +x"
			}
			fmt.Printf("  %-24s  action=%s  ext=%s%s\n", name, base, ifEmpty(ext, "(none)"), perm)
			any = true
		}
	}
	if !any {
		fmt.Printf("(no plugins in %s — files are <action>.<ext>; ext in .sh .py .go .js .ts or chmod+x bare binary)\n", dir)
	}
}

// ── Small utilities ────────────────────────────────────────────────────────

func newRequestID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("wreq-%d", time.Now().UnixNano())
	}
	return "wreq-" + hex.EncodeToString(b[:])
}

func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func prettyJSON(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) == nil {
		if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(pretty)
		}
	}
	return string(body)
}

// suppress "imported and not used" if our helpers shrink — these are used
// transitively by readFirstLine/openDB above.
var _ = sql.ErrNoRows
