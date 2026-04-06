package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeUntrusted_KeepsNormalText(t *testing.T) {
	input := "Hello World 你好世界\nNew line\ttab"
	got := sanitizeUntrusted(input)
	if got != input {
		t.Errorf("sanitized normal text changed: %q", got)
	}
}

func TestSanitizeUntrusted_StripsControlChars(t *testing.T) {
	input := "hello\x00world\x01test\x7f"
	got := sanitizeUntrusted(input)
	if got != "helloworldtest" {
		t.Errorf("expected control chars stripped, got %q", got)
	}
}

func TestSanitizeUntrusted_StripsFormatChars(t *testing.T) {
	// Zero-width space (U+200B), BOM (U+FEFF), Bi-di override (U+202E)
	input := "hello\u200Bworld\uFEFF\u202Etest"
	got := sanitizeUntrusted(input)
	if got != "helloworldtest" {
		t.Errorf("expected format chars stripped, got %q (len %d)", got, len(got))
	}
}

func TestSanitizeUntrusted_ConvertsLineSeparators(t *testing.T) {
	input := "line1\u2028line2\u2029line3"
	got := sanitizeUntrusted(input)
	if got != "line1\nline2\nline3" {
		t.Errorf("expected line separators converted to \\n, got %q", got)
	}
}

func TestSanitizeUntrusted_PreservesNewlineTabCR(t *testing.T) {
	input := "a\nb\rc\td"
	got := sanitizeUntrusted(input)
	if got != input {
		t.Errorf("should preserve \\n \\r \\t, got %q", got)
	}
}

func TestWrapUntrusted(t *testing.T) {
	input := "hello <script>alert('xss')</script>"
	got := wrapUntrusted(input)
	if !strings.HasPrefix(got, "<untrusted-text>") {
		t.Error("missing opening tag")
	}
	if !strings.HasSuffix(got, "\n</untrusted-text>") {
		t.Error("missing closing tag")
	}
	if strings.Contains(got, "<script>") {
		t.Error("< should be escaped")
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("< > not escaped properly: %s", got)
	}
}

func TestWrapUntrusted_AlsoSanitizes(t *testing.T) {
	input := "clean\x00dirty"
	got := wrapUntrusted(input)
	if strings.Contains(got, "\x00") {
		t.Error("control chars should be stripped before wrapping")
	}
}

func TestUnicodeCategory_ControlChars(t *testing.T) {
	tests := []struct {
		r    rune
		want string
	}{
		{0x00, "Cc"},
		{0x1F, "Cc"},
		{0x7F, "Cc"},
		{0x9F, "Cc"},
		{'A', ""},
		{'中', ""},
	}
	for _, tt := range tests {
		got := unicodeCategory(tt.r)
		if got != tt.want {
			t.Errorf("unicodeCategory(%U) = %q, want %q", tt.r, got, tt.want)
		}
	}
}

func TestUnicodeCategory_FormatChars(t *testing.T) {
	tests := []rune{0xAD, 0x200B, 0x200C, 0x200D, 0x200E, 0x200F, 0x202A, 0x2060, 0xFEFF}
	for _, r := range tests {
		got := unicodeCategory(r)
		if got != "Cf" {
			t.Errorf("unicodeCategory(%U) = %q, want 'Cf'", r, got)
		}
	}
}

func TestLoadFileWithBudget_SmallFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "small.md")
	os.WriteFile(f, []byte("hello world"), 0644)

	content, ok := loadFileWithBudget(f, 1000)
	if !ok {
		t.Error("should return ok")
	}
	if content != "hello world" {
		t.Errorf("content = %q", content)
	}
}

func TestLoadFileWithBudget_Truncation(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "big.md")
	// Create content with newlines
	lines := ""
	for i := 0; i < 100; i++ {
		lines += "line content here\n"
	}
	os.WriteFile(f, []byte(lines), 0644)

	content, ok := loadFileWithBudget(f, 50)
	if !ok {
		t.Error("should return ok even when truncated")
	}
	if !strings.Contains(content, "⚠️ [file truncated") {
		t.Errorf("truncated content should have warning, got: %s", content[:min(len(content), 200)])
	}
	if len(content) > 200 { // 50 chars + warning
		t.Errorf("content too long after truncation: %d", len(content))
	}
}

func TestLoadFileWithBudget_NonExistent(t *testing.T) {
	_, ok := loadFileWithBudget("/nonexistent/file", 1000)
	if ok {
		t.Error("should return not ok for nonexistent file")
	}
}

func TestEstimateTokens_Empty(t *testing.T) {
	if estimateTokens("") != 0 {
		t.Error("empty string should be 0 tokens")
	}
}

func TestEstimateTokens_English(t *testing.T) {
	// ~400 ASCII chars → ~100 tokens
	text := strings.Repeat("abcd", 100)
	tokens := estimateTokens(text)
	if tokens < 80 || tokens > 120 {
		t.Errorf("400 ASCII chars: expected ~100 tokens, got %d", tokens)
	}
}

func TestEstimateTokens_Chinese(t *testing.T) {
	// 100 Chinese chars → ~66 tokens (100 * 2/3)
	text := strings.Repeat("中", 100)
	tokens := estimateTokens(text)
	if tokens < 50 || tokens > 80 {
		t.Errorf("100 Chinese chars: expected ~66 tokens, got %d", tokens)
	}
}

func TestEstimateTokens_Mixed(t *testing.T) {
	text := "Hello World 你好世界"
	tokens := estimateTokens(text)
	if tokens < 3 || tokens > 10 {
		t.Errorf("mixed text: expected 3-10 tokens, got %d", tokens)
	}
}

func TestWritePrompt_SmallPrompt(t *testing.T) {
	origOut := promptOut
	dir := t.TempDir()
	promptOut = filepath.Join(dir, "prompt.md")
	defer func() { promptOut = origOut }()

	content := "small test prompt"
	writePrompt(buildPromptResult{content: content})

	data, err := os.ReadFile(promptOut)
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	if string(data) != content {
		t.Errorf("written content mismatch: %q", data)
	}
}

func TestWritePrompt_LargePrompt(t *testing.T) {
	origOut := promptOut
	dir := t.TempDir()
	promptOut = filepath.Join(dir, "prompt.md")
	defer func() { promptOut = origOut }()

	// Create a prompt that exceeds the token limit
	// 100k tokens ~= 250k ASCII chars
	content := strings.Repeat("test content here ", 20000) // ~360k chars
	writePrompt(buildPromptResult{content: content})

	// Should still write the file (just warns on stderr)
	if _, err := os.Stat(promptOut); err != nil {
		t.Error("prompt file should be written even when over limit")
	}
}

func TestBuildPrompt_ContainsTelegramSection(t *testing.T) {
	result := buildPrompt()
	if !strings.Contains(result.content, "Telegram") {
		t.Error("prompt should mention Telegram")
	}
}

func TestBuildPrompt_ContainsStartupProtocol(t *testing.T) {
	result := buildPrompt()
	// Check for either custom BOOT.md content or default boot protocol
	if !strings.Contains(result.content, "Boot Protocol") && !strings.Contains(result.content, "启动协议") {
		t.Error("prompt missing startup protocol")
	}
}

func TestBuildPrompt_HasReasonableSize(t *testing.T) {
	result := buildPrompt()
	tokens := estimateTokens(result.content)
	t.Logf("prompt: %d chars, ~%d tokens", len(result.content), tokens)
	if tokens > promptTokenLimit {
		t.Errorf("prompt exceeds token limit: %d > %d", tokens, promptTokenLimit)
	}
	if tokens < 1000 {
		t.Errorf("prompt suspiciously small: %d tokens", tokens)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
