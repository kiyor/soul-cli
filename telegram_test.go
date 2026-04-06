package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetTelegramToken_FromConfig(t *testing.T) {
	token := getTelegramToken()
	if token == "" {
		t.Skip("no telegram token configured (not on Kiyor's machine)")
	}
	if len(token) < 20 {
		t.Errorf("token too short: %d chars", len(token))
	}
}

// setTestHome overrides both home and appHome for test isolation
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	origHome := home
	origWeiranHome := appHome
	origCachedToken := cachedTelegramToken
	home = dir
	appHome = filepath.Join(dir, ".openclaw")
	cachedTelegramToken = "" // clear cache for test isolation
	t.Cleanup(func() {
		home = origHome
		appHome = origWeiranHome
		cachedTelegramToken = origCachedToken
	})
}

func TestGetTelegramToken_MissingConfig(t *testing.T) {
	setTestHome(t, "/nonexistent")

	token := getTelegramToken()
	if token != "" {
		t.Errorf("expected empty token for missing config, got %q", token)
	}
}

func TestGetTelegramToken_BadJSON(t *testing.T) {
	dir := t.TempDir()
	setTestHome(t, dir)

	ocDir := filepath.Join(dir, ".openclaw")
	os.MkdirAll(ocDir, 0755)
	os.WriteFile(filepath.Join(ocDir, "openclaw.json"), []byte("not json"), 0644)

	token := getTelegramToken()
	if token != "" {
		t.Errorf("expected empty token for bad JSON, got %q", token)
	}
}

func TestGetTelegramToken_EmptyConfig(t *testing.T) {
	dir := t.TempDir()
	setTestHome(t, dir)

	ocDir := filepath.Join(dir, ".openclaw")
	os.MkdirAll(ocDir, 0755)
	os.WriteFile(filepath.Join(ocDir, "openclaw.json"), []byte(`{}`), 0644)

	token := getTelegramToken()
	if token != "" {
		t.Errorf("expected empty token for empty config, got %q", token)
	}
}

func TestGetTelegramToken_NestedAccounts(t *testing.T) {
	dir := t.TempDir()
	setTestHome(t, dir)

	ocDir := filepath.Join(dir, ".openclaw")
	os.MkdirAll(ocDir, 0755)
	config := `{"channels":{"telegram":{"accounts":{"main":{"botToken":"test-token-12345678901234567890"}}}}}`
	os.WriteFile(filepath.Join(ocDir, "openclaw.json"), []byte(config), 0644)

	token := getTelegramToken()
	if token != "test-token-12345678901234567890" {
		t.Errorf("expected nested token, got %q", token)
	}
}

func TestGetTelegramToken_DirectToken(t *testing.T) {
	dir := t.TempDir()
	setTestHome(t, dir)

	ocDir := filepath.Join(dir, ".openclaw")
	os.MkdirAll(ocDir, 0755)
	config := `{"channels":{"telegram":{"botToken":"direct-token-99999999999999999999"}}}`
	os.WriteFile(filepath.Join(ocDir, "openclaw.json"), []byte(config), 0644)

	token := getTelegramToken()
	if token != "direct-token-99999999999999999999" {
		t.Errorf("expected direct token, got %q", token)
	}
}

func TestTrySendTelegram_NoToken(t *testing.T) {
	setTestHome(t, "/nonexistent")

	err := trySendTelegram("test message")
	if err == nil {
		t.Error("expected error when no token")
	}
}

func TestTrySendTelegramPhoto_NoToken(t *testing.T) {
	setTestHome(t, "/nonexistent")

	err := trySendTelegramPhoto("https://example.com/img.jpg", "caption")
	if err == nil {
		t.Error("expected error when no token")
	}
}
