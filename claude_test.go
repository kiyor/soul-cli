package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMustLock_CreatesLockFile(t *testing.T) {
	origLock := lockfile
	dir := t.TempDir()
	lockfile = filepath.Join(dir, "test.lock")
	defer func() {
		os.Remove(lockfile)
		lockfile = origLock
	}()

	// mustLock creates the file — but it also sets up signal handlers
	// and would exit if another instance is running. Test the file creation part.
	mustLock()

	data, err := os.ReadFile(lockfile)
	if err != nil {
		t.Fatalf("lock file not created: %v", err)
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("lock file should contain pid, got %q", data)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}

	releaseLock()

	if _, err := os.Stat(lockfile); !os.IsNotExist(err) {
		t.Error("lock file should be removed after release")
	}
}

func TestReleaseLock_NonExistent(t *testing.T) {
	origLock := lockfile
	lockfile = filepath.Join(t.TempDir(), "nonexistent.lock")
	defer func() { lockfile = origLock }()

	// Should not panic
	releaseLock()
}

func TestMustLock_StaleLock(t *testing.T) {
	origLock := lockfile
	dir := t.TempDir()
	lockfile = filepath.Join(dir, "stale.lock")
	defer func() {
		os.Remove(lockfile)
		lockfile = origLock
	}()

	// Write a stale PID (99999999 — very unlikely to be running)
	os.WriteFile(lockfile, []byte("99999999"), 0644)

	// mustLock should detect the stale lock and replace it
	mustLock()

	data, _ := os.ReadFile(lockfile)
	pid, _ := strconv.Atoi(string(data))
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want current pid %d (stale lock not cleaned)", pid, os.Getpid())
	}

	releaseLock()
}

func TestCronTimeout(t *testing.T) {
	origMode := currentMode
	defer func() { currentMode = origMode }()

	// heartbeat always 5m
	currentMode = "heartbeat"
	if got := cronTimeout(); got != 5*time.Minute {
		t.Errorf("heartbeat timeout = %v, want 5m", got)
	}

	// cron: 10m or 20m depending on day of week (Sunday = weekly)
	currentMode = "cron"
	got := cronTimeout()
	if time.Now().Weekday() == time.Sunday {
		if got != 20*time.Minute {
			t.Errorf("cron Sunday timeout = %v, want 20m", got)
		}
	} else {
		if got != 10*time.Minute {
			t.Errorf("cron weekday timeout = %v, want 10m", got)
		}
	}

	// default: 30m
	currentMode = "interactive"
	if got := cronTimeout(); got != 30*time.Minute {
		t.Errorf("interactive timeout = %v, want 30m", got)
	}
}

func TestGetModelEndpoint(t *testing.T) {
	// Create a temp openclaw.json
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "openclaw.json")

	cfg := map[string]interface{}{
		"agents": map[string]interface{}{
			"defaults": map[string]interface{}{
				"model": map[string]interface{}{
					"primary":   "zai/glm-5.1",
					"fallbacks": []string{"minimax/MiniMax-M2.7"},
				},
			},
		},
		"models": map[string]interface{}{
			"providers": map[string]interface{}{
				"minimax": map[string]interface{}{
					"baseUrl": "https://api.minimax.chat/v1",
				},
			},
		},
	}
	data, _ := json.Marshal(cfg)
	os.WriteFile(cfgPath, data, 0644)

	origPath := openclawConfigPath
	openclawConfigPath = cfgPath
	defer func() { openclawConfigPath = origPath }()

	got := getModelEndpoint()
	want := "https://api.minimax.chat/v1/models"
	if got != want {
		t.Errorf("getModelEndpoint() = %q, want %q", got, want)
	}
}

func TestGetModelEndpoint_NoCfg(t *testing.T) {
	origPath := openclawConfigPath
	openclawConfigPath = "/nonexistent/openclaw.json"
	defer func() { openclawConfigPath = origPath }()

	got := getModelEndpoint()
	if got != "" {
		t.Errorf("getModelEndpoint() = %q, want empty", got)
	}
}

func TestPreflight_ClaudeMissing(t *testing.T) {
	origBin := claudeBin
	claudeBin = "/nonexistent/claude"
	defer func() { claudeBin = origBin }()

	warnings := preflight()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "claude") && strings.Contains(w, "not found") {
			found = true
		}
	}
	if !found {
		t.Errorf("preflight should warn about missing claude binary, got: %v", warnings)
	}
}

func TestRotateMetrics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")

	// Write 15 lines
	var content string
	for i := 0; i < 15; i++ {
		content += `{"mode":"test","exit_code":0}` + "\n"
	}
	os.WriteFile(path, []byte(content), 0644)

	// Rotate with max=10, keep=5
	rotateMetrics(path, 10, 5)

	data, _ := os.ReadFile(path)
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 5 {
		t.Errorf("after rotation got %d lines, want 5", lines)
	}
}

func TestMustLock_BadPidInLockFile(t *testing.T) {
	origLock := lockfile
	dir := t.TempDir()
	lockfile = filepath.Join(dir, "bad.lock")
	defer func() {
		os.Remove(lockfile)
		lockfile = origLock
	}()

	// Write non-numeric content
	os.WriteFile(lockfile, []byte("not-a-pid"), 0644)

	// mustLock should handle this gracefully
	mustLock()

	data, _ := os.ReadFile(lockfile)
	pid, _ := strconv.Atoi(string(data))
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want current pid %d", pid, os.Getpid())
	}

	releaseLock()
}
