package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ── Lock ──

func mustLock() {
	// prefer mkdir atomic lock (NFS-safe), lockfile becomes directory
	lockDir := lockfile + ".d"

	// check if existing lock is stale
	if pid, ok := readLockPid(lockDir); ok {
		if proc, err := os.FindProcess(pid); err == nil {
			if err := proc.Signal(syscall.Signal(0)); err == nil {
				fmt.Fprintf(os.Stderr, "["+appName+"] another instance is running (pid %d), skipping\n", pid)
				os.Exit(0)
			}
		}
		// stale lock, clean up
		mkdirUnlock(lockDir)
	}

	if !mkdirLock(lockDir) {
		// mkdir failed but readLockPid also failed → possibly a leftover empty directory
		os.RemoveAll(lockDir)
		if !mkdirLock(lockDir) {
			fmt.Fprintf(os.Stderr, "["+appName+"] unable to acquire lock: %s\n", lockDir)
			os.Exit(1)
		}
	}

	// compatibility: also write old-format lockfile (for old version detection)
	os.WriteFile(lockfile, []byte(strconv.Itoa(os.Getpid())), 0644)

	// register signal handler to auto-clean lock files on kill
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\n["+appName+"] received signal %v, cleaning up lock files\n", sig)
		releaseLock()
		os.Exit(130) // 128 + SIGINT(2)
	}()
}

func releaseLock() {
	mkdirUnlock(lockfile + ".d")
	os.Remove(lockfile) // compatibility with old format
}

// ── exec claude ──

// execClaude replaces current process (for interactive/print modes, does not return)
func execClaude(args []string) {
	os.Chdir(workspace)

	bin, err := exec.LookPath(claudeBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] claude not found: %v\n", err)
		os.Exit(1)
	}

	argv := append([]string{"claude"}, args...)
	env := injectProxyEnv(os.Environ())
	// Mark as SDK entrypoint so Claude Code skips the interactive OAuth login check.
	// Without this, the login check hits ANTHROPIC_BASE_URL (our proxy) for auth
	// validation, which can fail because the proxy only forwards /v1/messages.
	env = append(env, "CLAUDE_CODE_ENTRYPOINT=sdk-go")

	if err := syscall.Exec(bin, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] exec failed: %v\n", err)
		os.Exit(1)
	}
}

// execClaudeResume directly resumes a session (no soul prompt added, already in session).
// Any extra args (e.g. --chrome) are passed through to claude.
func execClaudeResume(sessionID string, passthrough ...string) {
	args := []string{"--dangerously-skip-permissions", "--resume", sessionID}
	args = append(args, passthrough...)
	if len(passthrough) > 0 {
		fmt.Fprintf(os.Stderr, "["+appName+"] resuming session %s with %v...\n", shortID(sessionID), passthrough)
	} else {
		fmt.Fprintf(os.Stderr, "["+appName+"] resuming session %s...\n", shortID(sessionID))
	}
	execClaude(args)
}

// runClaude runs claude as subprocess (for cron/heartbeat, returns after completion, continues with hooks)
// cron/heartbeat have timeout protection (default 10 min) to prevent infinite waits when proxy is unavailable.
func runClaude(args []string) int {
	os.Chdir(workspace)

	cmd := exec.Command(claudeBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(injectProxyEnv(os.Environ()), "CLAUDE_CODE_ENTRYPOINT=sdk-go")

	// timeout protection: set timeout for cron/heartbeat modes
	timeout := cronTimeout()

	fmt.Fprintf(os.Stderr, "["+appName+"] starting claude (subprocess mode, timeout=%s)\n", timeout)
	start := time.Now()

	// start subprocess
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] claude start failed: %v\n", err)
		trackClaudeExit(1, 0, err.Error())
		return 1
	}

	// wait for completion or timeout
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	select {
	case err = <-done:
		// normal exit
	case <-time.After(timeout):
		// timeout: SIGTERM first, SIGKILL after 5 seconds
		fmt.Fprintf(os.Stderr, "["+appName+"] ⏰ claude timed out (%s), sending SIGTERM\n", timeout)
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err = <-done:
			// exited after SIGTERM
		case <-time.After(5 * time.Second):
			fmt.Fprint(os.Stderr, "["+appName+"] claude did not respond to SIGTERM, sending SIGKILL\n")
			cmd.Process.Kill()
			err = <-done
		}
		err = fmt.Errorf("timeout after %s", timeout)
	}

	duration := time.Since(start)

	exitCode := 0
	errMsg := ""
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			fmt.Fprintf(os.Stderr, "["+appName+"] claude exit code: %d\n", exitCode)
		} else {
			exitCode = 1
			errMsg = err.Error()
			fmt.Fprintf(os.Stderr, "["+appName+"] claude execution failed: %v\n", err)
		}
	} else {
		fmt.Fprint(os.Stderr, "["+appName+"] claude exited normally\n")
	}

	// persist exit code to metrics.jsonl for trend analysis
	trackClaudeExit(exitCode, duration, errMsg)
	return exitCode
}

// cronTimeout returns the timeout duration for cron/heartbeat
func cronTimeout() time.Duration {
	switch currentMode {
	case "heartbeat":
		return 5 * time.Minute
	case "cron":
		if time.Now().Weekday() == time.Sunday {
			return 20 * time.Minute // Sunday deep review needs more time
		}
		return 10 * time.Minute
	case "evolve":
		return 15 * time.Minute // self-evolution: more lenient than cron, may need compile+test
	default:
		return 30 * time.Minute // other modes: lenient timeout
	}
}

// preflight runs quick checks before starting claude in cron/heartbeat.
// Returns warning list (non-blocking, for logging only).
func preflight() []string {
	var warnings []string

	// 1. claude binary exists and is executable
	if _, err := os.Stat(claudeBin); err != nil {
		warnings = append(warnings, fmt.Sprintf("claude binary not found: %s", claudeBin))
	}

	// 2. check openclaw.json is readable
	if _, err := os.Stat(openclawConfigPath); err != nil {
		warnings = append(warnings, "openclaw.json not readable")
	}

	// 3. check model endpoint reachability (quick 2s timeout)
	endpoint := getModelEndpoint()
	if endpoint != "" {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("model endpoint unreachable: %s (%v)", endpoint, err))
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				warnings = append(warnings, fmt.Sprintf("model endpoint error: %s → %d", endpoint, resp.StatusCode))
			}
		}
	}

	// 4. disk space (macOS: /tmp and workspace partition)
	var stat syscall.Statfs_t
	if err := syscall.Statfs(workspace, &stat); err == nil {
		freeGB := float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		if freeGB < 1.0 {
			warnings = append(warnings, fmt.Sprintf("low disk space: %.1f GB remaining", freeGB))
		}
	}

	return warnings
}

// getModelEndpoint reads available provider baseURL from openclaw.json
// tries primary model then fallback models' providers in order
func getModelEndpoint() string {
	data, err := os.ReadFile(openclawConfigPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		Agents struct {
			Defaults struct {
				Model struct {
					Primary   string   `json:"primary"`
					Fallbacks []string `json:"fallbacks"`
				} `json:"model"`
			} `json:"defaults"`
		} `json:"agents"`
		Models struct {
			Providers map[string]struct {
				BaseURL string `json:"baseUrl"`
			} `json:"providers"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}

	// collect model IDs to check: primary + fallbacks
	candidates := []string{cfg.Agents.Defaults.Model.Primary}
	candidates = append(candidates, cfg.Agents.Defaults.Model.Fallbacks...)

	for _, model := range candidates {
		if model == "" {
			continue
		}
		parts := strings.SplitN(model, "/", 2)
		if len(parts) < 2 {
			continue
		}
		provider := parts[0]
		if p, ok := cfg.Models.Providers[provider]; ok && p.BaseURL != "" {
			return p.BaseURL + "/models"
		}
	}
	return ""
}

// providerConfig holds baseURL and apiKey for a model provider.
type providerConfig struct {
	BaseURL string   `json:"baseUrl"`
	APIKey  string   `json:"apiKey"`
	AuthEnv string   `json:"authEnv"` // env var name for the key, default "ANTHROPIC_AUTH_TOKEN"; use "ANTHROPIC_API_KEY" for MiniMax etc.
	Models  []string `json:"models"`  // available model names for this provider
}

// resolveProvider looks up a provider's endpoint config from config.json "providers" section.
// config.json is soul-cli's own config (in data/config.json).
func resolveProvider(providerName string) *providerConfig {
	all, _ := loadAllProviders()
	if prov, ok := all[providerName]; ok && prov.BaseURL != "" {
		return &prov
	}
	return nil
}

// loadAllProviders returns every provider from the first config.json found,
// along with the source path that was read.
func loadAllProviders() (map[string]providerConfig, string) {
	configPaths := []string{
		filepath.Join(appHome, "data", "config.json"),
		filepath.Join(workspace, "scripts", appName, "config.json"),
	}
	for _, p := range configPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			Providers map[string]providerConfig `json:"providers"`
		}
		if json.Unmarshal(data, &cfg) == nil && len(cfg.Providers) > 0 {
			return cfg.Providers, p
		}
	}
	return nil, ""
}

// injectModelOverrideEnv injects ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN for the overrideModel provider.
// Called by injectProxyEnv when overrideModel is set (CLI mode backward compat).
// Returns the modified env and whether the override was applied.
func injectModelOverrideEnv(env []string) ([]string, bool) {
	return injectProviderEnv(env, overrideModel)
}

// injectProviderEnv injects ANTHROPIC_BASE_URL + auth env for the given model's provider.
// model must be in "provider/modelName" format (e.g. "zai/glm-5.1").
// Returns the modified env and whether the override was applied.
func injectProviderEnv(env []string, model string) ([]string, bool) {
	if model == "" {
		return env, false
	}

	parts := strings.SplitN(model, "/", 2)
	if len(parts) < 2 {
		return env, false
	}

	providerName := parts[0]
	modelName := parts[1]
	provider := resolveProvider(providerName)
	if provider == nil {
		fmt.Fprintf(os.Stderr, "[%s] provider %q not found in config.json (try `%s models`)\n", appName, providerName, appName)
		return env, false
	}

	// Validate model name against provider's whitelist.
	// Some upstreams (like MiniMax) silently fall back to a default model when
	// an unknown name is passed, which leads to "it runs but with the wrong model".
	// We warn loudly so typos get caught before the first token is generated.
	if len(provider.Models) > 0 {
		found := false
		for _, m := range provider.Models {
			if m == modelName {
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "[%s] ⚠️  model %q not registered under provider %q\n", appName, modelName, providerName)
			fmt.Fprintf(os.Stderr, "[%s]    available: ", appName)
			for i, m := range provider.Models {
				if i > 0 {
					fmt.Fprint(os.Stderr, ", ")
				}
				fmt.Fprintf(os.Stderr, "%s/%s", providerName, m)
			}
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "[%s]    upstream may silently fall back to a default model. (use `%s models` to list)\n", appName, appName)
		}
	}

	fmt.Fprintf(os.Stderr, "[%s] using provider %q → %s\n", appName, providerName, provider.BaseURL)

	// Filter out conflicting env vars
	filtered := make([]string, 0, len(env)+3)
	for _, v := range env {
		if strings.HasPrefix(v, "ANTHROPIC_BASE_URL=") ||
			strings.HasPrefix(v, "ANTHROPIC_AUTH_TOKEN=") ||
			strings.HasPrefix(v, "ANTHROPIC_API_KEY=") {
			continue
		}
		filtered = append(filtered, v)
	}

	filtered = append(filtered, "ANTHROPIC_BASE_URL="+provider.BaseURL)
	if provider.APIKey != "" {
		authEnv := provider.AuthEnv
		if authEnv == "" {
			authEnv = "ANTHROPIC_AUTH_TOKEN"
		}
		filtered = append(filtered, authEnv+"="+provider.APIKey)
	}
	// Extend timeout for third-party providers (they can be slower)
	filtered = append(filtered, "API_TIMEOUT_MS=3000000")

	return filtered, true
}

// providerModelName extracts the model name from a "provider/model" string.
// Returns the full string if no "/" is found (Anthropic native model).
func providerModelName(model string) string {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// isProviderModel returns true if the model string is in "provider/model" format.
func isProviderModel(model string) bool {
	return strings.Contains(model, "/")
}

// trackClaudeExit appends claude subprocess exit info to metrics.jsonl
func trackClaudeExit(exitCode int, duration time.Duration, errMsg string) {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")

	rec := struct {
		Timestamp string  `json:"ts"`
		Mode      string  `json:"mode"`
		ExitCode  int     `json:"exit_code"`
		Duration  float64 `json:"duration_s"`
		Error     string  `json:"error,omitempty"`
	}{
		Timestamp: time.Now().Format(time.RFC3339),
		Mode:      currentMode,
		ExitCode:  exitCode,
		Duration:  duration.Seconds(),
		Error:     errMsg,
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	data = append(data, '\n')

	f, err := os.OpenFile(metricsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	f.Write(data)
	fi, _ := f.Stat()
	f.Close()

	// rotate only if file is large enough to possibly exceed 1000 lines
	// (average metric line ~150 bytes, so 1000 lines ~150KB)
	if fi != nil && fi.Size() > 100*1024 {
		rotateMetrics(metricsPath, 1000, 800)
	}
}

// rotateMetrics truncates to last keepLines when file exceeds maxLines
func rotateMetrics(path string, maxLines, keepLines int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) <= maxLines {
		return
	}
	kept := lines[len(lines)-keepLines:]
	os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0644)
}
