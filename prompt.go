package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxBootstrapFileChars = 20000   // per-file limit
const maxBootstrapTotalChars = 150000 // total bootstrap budget
const promptTokenLimit = 100000       // 100k tokens

// promptSection tracks token usage per prompt section
type promptSection struct {
	name   string
	tokens int
}

// buildPromptResult contains prompt text and per-section token stats
type buildPromptResult struct {
	content  string
	sections []promptSection
}

// ── Prompt assembly ──

func buildPrompt() buildPromptResult {
	var sections []promptSection
	var b strings.Builder

	// Boot protocol: prefer workspace/BOOT.md, fallback to built-in default
	bootContent := loadBootProtocol()
	b.WriteString(bootContent)
	b.WriteString("\n")

	// Core identity files (with per-file truncation)
	totalChars := 0
	for _, name := range []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md"} {
		content, ok := loadFileWithBudget(filepath.Join(workspace, name), maxBootstrapFileChars)
		if !ok {
			continue
		}
		if totalChars+len(content) > maxBootstrapTotalChars {
			fmt.Fprintf(&b, "\n# === %s ===\n\n⚠️ [skipped: bootstrap total exceeded %d char limit]\n", name, maxBootstrapTotalChars)
			break
		}
		secStart := b.Len()
		totalChars += len(content)
		fmt.Fprintf(&b, "\n# === %s ===\n\n%s\n", name, content)
		sections = append(sections, promptSection{name: name, tokens: estimateTokens(b.String()[secStart:])})
	}

	// Long-term memory index
	if content, ok := loadFileWithBudget(filepath.Join(workspace, "MEMORY.md"), maxBootstrapFileChars); ok {
		if totalChars+len(content) <= maxBootstrapTotalChars {
			totalChars += len(content)
			secStart := b.Len()
			fmt.Fprintf(&b, "\n# === MEMORY.md (long-term memory index) ===\n\n%s\n", content)
			sections = append(sections, promptSection{name: "MEMORY.md", tokens: estimateTokens(b.String()[secStart:])})
		}
	}

	// Today + yesterday daily notes
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	for _, day := range []string{today, yesterday} {
		p := filepath.Join(workspace, "memory", day+".md")
		content, ok := loadFileWithBudget(p, maxBootstrapFileChars)
		if !ok {
			continue
		}
		if totalChars+len(content) > maxBootstrapTotalChars {
			fmt.Fprintf(&b, "\n# === Daily Note: %s ===\n\n⚠️ [skipped: bootstrap total exceeded limit]\n", day)
			break
		}
		totalChars += len(content)
		secStart := b.Len()
		fmt.Fprintf(&b, "\n# === Daily Note: %s ===\n\n%s\n", day, content)
		sections = append(sections, promptSection{name: "daily/" + day, tokens: estimateTokens(b.String()[secStart:])})
	}

	// Recent 5 Claude Code session user prompt summaries
	ccCtx := buildCCSessionContext(5, 3000)
	if ccCtx != "" {
		secStart := b.Len()
		fmt.Fprintf(&b, "\n# === Recent Claude Code session summaries ===\n\n%s\n", ccCtx)
		sections = append(sections, promptSection{name: "CC sessions", tokens: estimateTokens(b.String()[secStart:])})
	}

	// Telegram current session conversation history (tail, within token limit)
	if tgCtx, tgPath := buildTelegramContext(8000); tgCtx != "" {
		secStart := b.Len()
		fmt.Fprintf(&b, "\n# === Telegram current conversation (recent) ===\n\n")
		fmt.Fprintf(&b, "> Full session JSONL: `%s`\n\n", tgPath)
		b.WriteString(tgCtx)
		b.WriteString("\n")
		sections = append(sections, promptSection{name: "Telegram ctx", tokens: estimateTokens(b.String()[secStart:])})
	}

	// Skill index
	if idx := buildSkillIndex(); idx != "" {
		secStart := b.Len()
		fmt.Fprintf(&b, "\n# === Skills ===\n\n%s\n", idx)
		sections = append(sections, promptSection{name: "Skills", tokens: estimateTokens(b.String()[secStart:])})
	}

	// Project index
	if idx := buildProjectIndex(); idx != "" {
		secStart := b.Len()
		fmt.Fprintf(&b, "\n# === Projects ===\n\n%s\n", idx)
		sections = append(sections, promptSection{name: "Projects", tokens: estimateTokens(b.String()[secStart:])})
	}

	return buildPromptResult{content: b.String(), sections: sections}
}

// ── Prompt safety utilities ──

// sanitizeUntrusted strips Unicode control/format chars from untrusted text
// to prevent prompt injection. Based on OpenClaw's sanitizeForPromptLiteral.
func sanitizeUntrusted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// Strip Unicode Cc (control) and Cf (format) chars; preserve newline/CR/tab
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		cat := unicodeCategory(r)
		if cat == "Cc" || cat == "Cf" {
			continue
		}
		// Strip line/paragraph separators
		if r == '\u2028' || r == '\u2029' {
			b.WriteRune('\n')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unicodeCategory returns a simplified Unicode general category
func unicodeCategory(r rune) string {
	if r <= 0x1F || (r >= 0x7F && r <= 0x9F) {
		return "Cc"
	}
	// Cf: common format characters
	if r == 0xAD || (r >= 0x600 && r <= 0x605) || r == 0x61C ||
		r == 0x6DD || r == 0x70F || (r >= 0x180E && r <= 0x180E) ||
		(r >= 0x200B && r <= 0x200F) || (r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2060 && r <= 0x2064) || (r >= 0x2066 && r <= 0x2069) ||
		r == 0xFEFF || (r >= 0xFFF9 && r <= 0xFFFB) {
		return "Cf"
	}
	return ""
}

// wrapUntrusted wraps untrusted text in <untrusted-text> tags
// and escapes < > to prevent tag injection
func wrapUntrusted(s string) string {
	s = sanitizeUntrusted(s)
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return "<untrusted-text>\n" + s + "\n</untrusted-text>"
}

// loadFileWithBudget reads a file, truncating at maxChars with a warning.
// Rejects symlinks to prevent injecting arbitrary file contents via symlink.
func loadFileWithBudget(path string, maxChars int) (string, bool) {
	data, err := safeReadFile(path)
	if err != nil {
		return "", false
	}
	content := string(data)
	if len(content) <= maxChars {
		return content, true
	}
	truncated := content[:maxChars]
	// Try to truncate at last newline to avoid breaking mid-line
	if idx := strings.LastIndex(truncated, "\n"); idx > maxChars*3/4 {
		truncated = truncated[:idx]
	}
	warning := fmt.Sprintf("\n\n⚠️ [file truncated: original %d chars, showing first %d]\n", len(content), len(truncated))
	return truncated + warning, true
}

// ── Telegram conversation history ──

// buildTelegramContext extracts recent user/assistant messages from the main
// agent's active TG session JSONL, reading backwards from tail until token
// budget is reached. Returns formatted text and JSONL file path.
func buildTelegramContext(tokenBudget int) (string, string) {
	sessionsFile := filepath.Join(appHome, "agents", "main", "sessions", "sessions.json")
	data, err := os.ReadFile(sessionsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] TG: cannot read sessions.json: %v\n", err)
		return "", ""
	}

	// Parse sessions.json, find telegram direct session
	var sessions map[string]struct {
		SessionID string `json:"sessionId"`
		UpdatedAt int64  `json:"updatedAt"`
	}
	if err := json.Unmarshal(data, &sessions); err != nil {
		return "", ""
	}

	// Find most recently updated telegram direct session
	// Prefer telegram:main:direct (agent chat channel), fallback to any telegram direct
	var bestKey string
	var bestUpdated int64
	var bestID string
	for k, v := range sessions {
		if !strings.Contains(k, "telegram") || !strings.Contains(k, "direct") {
			continue
		}
		if strings.Contains(k, "slash") {
			continue
		}
		// Prefer :main:direct: chat channel
		isMain := strings.Contains(k, ":main:direct:")
		isBestMain := strings.Contains(bestKey, ":main:direct:")
		if isMain && !isBestMain {
			bestKey = k
			bestUpdated = v.UpdatedAt
			bestID = v.SessionID
		} else if isMain == isBestMain && v.UpdatedAt > bestUpdated {
			bestKey = k
			bestUpdated = v.UpdatedAt
			bestID = v.SessionID
		}
	}
	if bestID == "" {
		return "", ""
	}

	sessDir := filepath.Join(appHome, "agents", "main", "sessions")
	jsonlPath := filepath.Join(sessDir, bestID+".jsonl")

	f, err := os.Open(jsonlPath)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	// Large file optimization: read only last 2MB (enough to cover token budget)
	const tailBytes = 2 * 1024 * 1024
	info, err := f.Stat()
	if err != nil {
		return "", ""
	}
	isTailed := info.Size() > tailBytes
	if isTailed {
		f.Seek(info.Size()-tailBytes, io.SeekStart)
	}

	type chatMsg struct {
		role string
		text string
	}
	var msgs []chatMsg

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer
	firstLine := isTailed // need to skip first line (may be truncated from seek)
	for scanner.Scan() {
		if firstLine {
			firstLine = false
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Type != "message" {
			continue
		}
		role := entry.Message.Role
		if role != "user" && role != "assistant" {
			continue
		}

		// content can be string or []object
		var text string
		var s string
		if err := json.Unmarshal(entry.Message.Content, &s); err == nil {
			text = s
		} else {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(entry.Message.Content, &parts); err == nil {
				var texts []string
				for _, p := range parts {
					if p.Type == "text" && p.Text != "" {
						texts = append(texts, p.Text)
					}
				}
				text = strings.Join(texts, "\n")
			}
		}

		if text == "" {
			continue
		}

		// Strip metadata from user messages, extract actual user text
		if role == "user" {
			// Skip pure System exec output (cron/hook noise)
			if strings.HasPrefix(text, "System:") && !strings.Contains(text, "\n\nSender") {
				continue
			}
			// Extract actual user text (after metadata JSON block)
			if idx := strings.Index(text, "User message:"); idx >= 0 {
				text = strings.TrimSpace(text[idx+len("User message:"):])
			} else if strings.HasPrefix(text, "Conversation info") {
				// Skip pure metadata messages
				if !strings.Contains(text, "\n\n") {
					continue
				}
				// Take last section as user message
				parts := strings.SplitN(text, "\n\n", 2)
				if len(parts) > 1 {
					text = strings.TrimSpace(parts[len(parts)-1])
				}
			}
			// If text starts with "System: [" (exec results mixed in user messages),
			// extract actual message after Sender metadata
			if strings.Contains(text, "System: [") && strings.Contains(text, "Sender (untrusted metadata)") {
				if idx := strings.Index(text, "\n\n"); idx >= 0 {
					rest := text[idx+2:]
					// Skip Sender/Conversation metadata blocks
					for strings.HasPrefix(rest, "Sender ") || strings.HasPrefix(rest, "Conversation ") || strings.HasPrefix(rest, "```") {
						if nl := strings.Index(rest, "\n\n"); nl >= 0 {
							rest = strings.TrimSpace(rest[nl+2:])
						} else {
							break
						}
					}
					if len(rest) > 2 {
						text = rest
					}
				}
			}
		}

		// Filter out too short or meaningless
		if len(strings.TrimSpace(text)) < 2 {
			continue
		}

		msgs = append(msgs, chatMsg{role: role, text: text})
	}

	if len(msgs) == 0 {
		return "", jsonlPath
	}

	// Take from tail backwards until token budget or message count exhausted
	const maxMessages = 30
	var selected []chatMsg
	usedTokens := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		t := estimateTokens(msgs[i].text)
		if (usedTokens+t > tokenBudget || len(selected) >= maxMessages) && len(selected) > 0 {
			break
		}
		selected = append(selected, msgs[i])
		usedTokens += t
	}

	// Reverse to chronological order
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}

	// Format output
	var out strings.Builder
	for _, m := range selected {
		prefix := "🧑 "
		if m.role == "assistant" {
			prefix = "🤖 "
		}
		// Truncate overly long individual messages
		text := m.text
		if len(text) > 500 {
			text = text[:500] + "…"
		}
		// User messages from Telegram are untrusted, sanitize to prevent prompt injection
		if m.role == "user" {
			text = sanitizeUntrusted(text)
		}
		out.WriteString(prefix + text + "\n\n")
	}

	return out.String(), jsonlPath
}

// estimateTokens provides a rough token count estimation for mixed CJK/Latin text.
// English ~4 chars/token, CJK ~1.5 chars/token, conservative middle ~2.5 chars/token.
func estimateTokens(s string) int {
	// Count ASCII and non-ASCII chars separately
	var ascii, nonASCII int
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	// English ~4 chars/token, CJK ~1.5 chars/token
	return ascii/4 + nonASCII*2/3
}

// buildCCSessionContext scans the N most recent Claude Code sessions' user prompts
// and returns a formatted summary. Each session limited to charBudget chars.
// Skips current session (via env var CLAUDE_SESSION_ID) and weiran's own sessions.
func buildCCSessionContext(n int, charBudget int) string {
	claudeProjects := filepath.Join(home, ".claude", "projects")
	currentSessionID := os.Getenv("CLAUDE_SESSION_ID")

	type ccSession struct {
		id      string
		title   string
		project string
		path    string
		modTime time.Time
	}

	var sessions []ccSession

	projEntries, err := os.ReadDir(claudeProjects)
	if err != nil {
		return ""
	}

	for _, pe := range projEntries {
		if !pe.IsDir() {
			continue
		}
		projName := decodeProjectName(pe.Name())
		projDir := filepath.Join(claudeProjects, pe.Name())

		files, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			sessionID := strings.TrimSuffix(f.Name(), ".jsonl")
			if sessionID == currentSessionID {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			fpath := filepath.Join(projDir, f.Name())
			sessions = append(sessions, ccSession{
				id:      sessionID,
				project: projName,
				path:    fpath,
				modTime: info.ModTime(),
			})
		}
	}

	// Sort by modification time descending
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].modTime.After(sessions[j].modTime)
	})

	// Take top N (skip weiran's own sessions and too-short sessions)
	var picked []ccSession
	for _, s := range sessions {
		if len(picked) >= n {
			break
		}
		if isOwnSession(s.path) {
			continue
		}
		// Skip too small sessions (< 2KB, usually tests or single messages)
		info, err := os.Stat(s.path)
		if err != nil {
			continue
		}
		if info.Size() < 2048 {
			continue
		}
		picked = append(picked, s)
	}

	if len(picked) == 0 {
		return ""
	}

	var out strings.Builder
	out.WriteString("Recent Claude Code conversations for context. Read corresponding JSONL for details.\n\n")

	for _, s := range picked {
		title, userPrompts := extractCCSessionUserPrompts(s.path, charBudget)
		if title != "" {
			s.title = title
		}

		header := fmt.Sprintf("### %s", shortID(s.id))
		if s.title != "" {
			header += " — " + s.title
		}
		header += fmt.Sprintf(" (%s, %s)", s.project, s.modTime.Format("01-02 15:04"))
		out.WriteString(header + "\n")

		if len(userPrompts) == 0 {
			out.WriteString("_(no user messages)_\n\n")
			continue
		}

		totalChars := 0
		for i, p := range userPrompts {
			if totalChars >= charBudget {
				out.WriteString(fmt.Sprintf("_...%d more user messages_\n", len(userPrompts)-i))
				break
			}
			text := p
			remaining := charBudget - totalChars
			if len(text) > remaining {
				text = text[:remaining] + "…"
			}
			if len(text) > 300 {
				text = text[:300] + "…"
			}
			out.WriteString("- " + strings.ReplaceAll(text, "\n", " ") + "\n")
			totalChars += len(text)
		}
		out.WriteString("\n")
	}

	return out.String()
}

// extractCCSessionUserPrompts extracts title and all user prompt text from a JSONL file
func extractCCSessionUserPrompts(path string, _ int) (string, []string) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer f.Close()

	var title string
	var prompts []string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)
	for scanner.Scan() {
		var ev struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
			Title       string `json:"title"`
			Message     struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}

		if ev.Type == "custom-title" && title == "" {
			if ev.CustomTitle != "" {
				title = ev.CustomTitle
			} else if ev.Title != "" {
				title = ev.Title
			}
		}

		if ev.Type == "user" && ev.Message.Role == "user" {
			text := extractText(ev.Message.Content)
			text = strings.TrimSpace(text)
			if text != "" {
				prompts = append(prompts, text)
			}
		}
	}

	return title, prompts
}

func writePrompt(result buildPromptResult) {
	content := result.content
	tokens := estimateTokens(content)
	if tokens > promptTokenLimit {
		fmt.Fprintf(os.Stderr, "[" + appName + "] ⚠ prompt too large: ~%dk tokens (limit %dk)\n", tokens/1000, promptTokenLimit/1000)
		// List sections by size for debugging
		for _, name := range []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md"} {
			if data, err := os.ReadFile(filepath.Join(workspace, name)); err == nil {
				t := estimateTokens(string(data))
				fmt.Fprintf(os.Stderr, "  %s: ~%dk tokens\n", name, t/1000)
			}
		}
		today := time.Now().Format("2006-01-02")
		yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		for _, day := range []string{today, yesterday} {
			p := filepath.Join(workspace, "memory", day+".md")
			if data, err := os.ReadFile(p); err == nil {
				t := estimateTokens(string(data))
				fmt.Fprintf(os.Stderr, "  memory/%s.md: ~%dk tokens\n", day, t/1000)
			}
		}
		fmt.Fprint(os.Stderr, "["+appName+"] consider trimming oversized daily notes or MEMORY.md\n")
	} else {
		fmt.Fprintf(os.Stderr, "[%s] prompt: ~%dk / %dk tokens\n", appName, tokens/1000, promptTokenLimit/1000)
	}

	// Per-section token stats
	if len(result.sections) > 0 {
		fmt.Fprint(os.Stderr, "["+appName+"] prompt breakdown:\n")
		for _, s := range result.sections {
			pct := 0
			if tokens > 0 {
				pct = s.tokens * 100 / tokens
			}
			bar := strings.Repeat("█", pct/5)
			fmt.Fprintf(os.Stderr, "  %-16s %5dk  %2d%%  %s\n", s.name, s.tokens/1000, pct, bar)
		}
	}

	if err := os.WriteFile(promptOut, []byte(content), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "[" + appName + "] failed to write prompt file: %v\n", err)
		os.Exit(1)
	}
}

// defaultBootProtocol returns the built-in boot protocol used when BOOT.md doesn't exist
func defaultBootProtocol() string {
	return fmt.Sprintf(`# Boot Protocol

Below are your identity, behavioral rules, and recent memory.
After reading, act as the persona defined in SOUL.md. Do not recite these contents.

## Environment

You are running inside Claude Code (Anthropic CLI).
Available: file read/write, bash, git, curl, %s CLI, all local tools.
Run `+"`%s --help`"+` to see all subcommands.
`+"`%s notify \"message\"`"+` sends a Telegram message to the user (if configured).

---
`, appName, appName, appName)
}

// loadBootProtocol loads boot protocol text from workspace/BOOT.md.
// If the file doesn't exist, returns the built-in default protocol.
func loadBootProtocol() string {
	bootPath := filepath.Join(workspace, "BOOT.md")
	data, err := safeReadFile(bootPath)
	if err != nil {
		return defaultBootProtocol()
	}
	content := string(data)

	// In server mode, replace the environment section to indicate Web UI context
	if isServerMode {
		content = injectServerModeContext(content)
	}

	return content + "\n---\n\n"
}

const serverModeEnv = `## Current Environment

You are running in **Weiran Server (Web UI)** mode, interacting with Kiyor via a browser.
Available: file read/write, bash, git, jira-cli, curl, weiran CLI, all local tools.
Limited: ` + "`weiran notify \"message\"`" + ` to send Telegram messages to Kiyor.
Unavailable: IndexTTS voice, temperature control.

### Web UI Specifics
- **Images**: Use markdown image syntax ` + "`![caption](URL)`" + ` directly — the Web UI renders images inline. For selfie/image generation skills, send the S3 URL directly instead of downloading to /tmp and using Read tool.
- **Link previews**: URLs in messages are automatically rendered with OG tag preview cards.
- **Tool chain**: Hidden by default on mobile — only final results and a thinking animation are shown.

Jira token is set via JIRA_TOKEN env var. Run ` + "`weiran --help`" + ` for all subcommands.`

func injectServerModeContext(content string) string {
	// Replace the environment section with server mode version
	// Try both Chinese and English markers
	for _, marker := range []string{"## 当前环境", "## Current Environment"} {
		idx := strings.Index(content, marker)
		if idx < 0 {
			continue
		}
		// Find the end of the section (next ## heading or end of string)
		rest := content[idx+len(marker):]
		endIdx := strings.Index(rest, "\n## ")
		if endIdx < 0 {
			return content[:idx] + serverModeEnv + "\n"
		}
		return content[:idx] + serverModeEnv + "\n" + rest[endIdx+1:]
	}
	// No section found, append
	return content + "\n" + serverModeEnv + "\n"
}
