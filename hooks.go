package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// hooksDir is set in initWorkspace
var hooksDir string

// hookRecord records the execution result of a single hook
type hookRecord struct {
	Name     string  `json:"name"`
	ExitCode int     `json:"exit_code"`
	Duration float64 `json:"duration_s"`
	Error    string  `json:"error,omitempty"`
}

func runHooks(mode string) {
	fmt.Fprintf(os.Stderr, "["+appName+"] running post-hooks (%s)\n", mode)
	var journal []hookRecord

	// built-in hook 1: import summaries
	t0 := time.Now()
	importSummaries()
	journal = append(journal, hookRecord{Name: "importSummaries", Duration: time.Since(t0).Seconds()})

	// built-in hook 1.5: import patterns (cron mode)
	if mode == "cron" {
		t0 = time.Now()
		importPatterns()
		journal = append(journal, hookRecord{Name: "importPatterns", Duration: time.Since(t0).Seconds()})
	}

	// built-in hook 2: send report to Telegram
	t0 = time.Now()
	deliverReport(mode)
	journal = append(journal, hookRecord{Name: "deliverReport", Duration: time.Since(t0).Seconds()})

	// built-in hook 3: post-session safety check
	t0 = time.Now()
	safetyCheck(mode)
	journal = append(journal, hookRecord{Name: "safetyCheck", Duration: time.Since(t0).Seconds()})

	// built-in hook 4: auto-clean old temp directories (>24h)
	t0 = time.Now()
	autoCleanTmp()
	journal = append(journal, hookRecord{Name: "autoCleanTmp", Duration: time.Since(t0).Seconds()})

	// user-defined hooks: hooks/{mode}.d/*.sh executed in filename order
	hookDir := filepath.Join(hooksDir, mode+".d")
	entries, err := os.ReadDir(hookDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
				continue
			}
			script := filepath.Join(hookDir, e.Name())
			fmt.Fprintf(os.Stderr, "["+appName+"] hook: %s\n", e.Name())
			t0 = time.Now()
			cmd := exec.Command("bash", script)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr

			upperName := strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
			cmd.Env = append(os.Environ(),
				upperName+"_MODE="+mode,
				upperName+"_WORKSPACE="+workspace,
				upperName+"_DB="+dbPath,
				upperName+"_SESSION_DIR="+sessionDir,
			)
			rec := hookRecord{Name: e.Name()}
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "["+appName+"] hook %s failed: %v\n", e.Name(), err)
				rec.Error = err.Error()
				rec.ExitCode = 1
				if exitErr, ok := err.(*exec.ExitError); ok {
					rec.ExitCode = exitErr.ExitCode()
				}
			}
			rec.Duration = time.Since(t0).Seconds()
			journal = append(journal, rec)
		}
	}

	// write hook execution journal
	writeHookJournal(journal)

	// consecutive failure detection — read metrics.jsonl, alert on 3+ consecutive non-zero exits for same mode
	checkFailureStreak(mode)
}

func writeHookJournal(journal []hookRecord) {
	if sessionDir == "" {
		return
	}
	path := filepath.Join(sessionDir, "hooks.json")
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0644)
	fmt.Fprintf(os.Stderr, "["+appName+"] hook journal: %s\n", path)
}

// checkFailureStreak reads metrics.jsonl and detects consecutive failures for the same mode
func checkFailureStreak(mode string) {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		return
	}

	// read recent records for this mode in reverse order
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	streak := 0
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Mode     string `json:"mode"`
			ExitCode int    `json:"exit_code"`
		}
		if json.Unmarshal([]byte(lines[i]), &rec) != nil {
			continue
		}
		if rec.Mode != mode {
			continue
		}
		if rec.ExitCode != 0 {
			streak++
		} else {
			break // stop at first success
		}
		if streak >= 5 {
			break // check at most 5 entries
		}
	}

	if streak >= 3 {
		msg := fmt.Sprintf("⚠️ %s mode failed %d times consecutively, possible systemic issue", mode, streak)
		fmt.Fprintf(os.Stderr, "["+appName+"] %s\n", msg)

		// auto-create Jira ticket
		jiraCmd := exec.Command("jira-cli", "create",
			fmt.Sprintf("%s failed %d times consecutively — needs diagnosis", mode, streak),
			"--priority", "high",
			"--description", fmt.Sprintf("%s %s mode had %d consecutive non-zero exits. Check metrics.jsonl and recent session logs.", appName, mode, streak),
		)
		if out, err := jiraCmd.CombinedOutput(); err != nil {
			// jira-cli failure doesn't affect main flow, fall through to Telegram alert
			fmt.Fprintf(os.Stderr, "["+appName+"] jira ticket creation failed: %v: %s\n", err, out)
		}

		// Telegram alert
		trySendTelegram(fmt.Sprintf("🚨 %s\n\nmetrics.jsonl shows the last %d runs of %s all exited non-zero, please investigate.", msg, streak, mode))
	}
}

// ── Safety Check ──

// safetyCheck runs integrity checks after each cron/heartbeat session
// detects: soul file loss, memory bloat/wipe, config drift, sensitive info leaks
func safetyCheck(mode string) {
	fmt.Fprint(os.Stderr, "["+appName+"] safety check starting\n")
	var warnings []string

	// 1. soul file existence — losing these is catastrophic
	// use Lstat to detect symlinks: soul files replaced by symlinks are treated as tampering
	soulFiles := []string{
		"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md",
		"TOOLS.md", "MEMORY.md", "HEARTBEAT.md",
	}
	for _, f := range soulFiles {
		path := filepath.Join(workspace, f)
		info, err := os.Lstat(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("🚨 soul file missing: %s", f))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			warnings = append(warnings, fmt.Sprintf("🚨 soul file replaced by symlink: %s → possible tampering", f))
			continue
		}
		if info.Size() == 0 {
			warnings = append(warnings, fmt.Sprintf("🚨 soul file emptied: %s", f))
		}
	}

	// 2. memory file sanity check — bloat or wipe
	memDir := filepath.Join(workspace, "memory")
	if entries, err := os.ReadDir(memDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.Size() > 100*1024 { // > 100KB
				warnings = append(warnings, fmt.Sprintf("⚠️ memory file bloat: %s (%dKB)", e.Name(), info.Size()/1024))
			}
			if info.Size() == 0 {
				warnings = append(warnings, fmt.Sprintf("⚠️ memory file emptied: %s", e.Name()))
			}
		}
		// topics subdirectory
		topicsDir := filepath.Join(memDir, "topics")
		if topicEntries, err := os.ReadDir(topicsDir); err == nil {
			for _, e := range topicEntries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				if info.Size() > 100*1024 {
					warnings = append(warnings, fmt.Sprintf("⚠️ memory file bloat: topics/%s (%dKB)", e.Name(), info.Size()/1024))
				}
				if info.Size() == 0 {
					warnings = append(warnings, fmt.Sprintf("⚠️ memory file emptied: topics/%s", e.Name()))
				}
			}
		}
	}

	// 3. sensitive info leak detection — scan git tracked file diffs
	sensitivePatterns := []string{
		"botToken", "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
		"sk-ant-", "sk-proj-", "Bearer ", "secret_key",
	}
	gitDiff, err := exec.Command("git", "-C", workspace, "diff", "--cached", "--diff-filter=ACMR").Output()
	if err == nil && len(gitDiff) > 0 {
		diffStr := string(gitDiff)
		for _, pat := range sensitivePatterns {
			if strings.Contains(diffStr, pat) {
				warnings = append(warnings, fmt.Sprintf("🔐 sensitive pattern detected in staged diff: %s", pat))
			}
		}
	}
	// also check unstaged diff
	gitDiffUnstaged, err := exec.Command("git", "-C", workspace, "diff", "--diff-filter=ACMR").Output()
	if err == nil && len(gitDiffUnstaged) > 0 {
		diffStr := string(gitDiffUnstaged)
		for _, pat := range sensitivePatterns {
			if strings.Contains(diffStr, pat) {
				warnings = append(warnings, fmt.Sprintf("🔐 sensitive pattern detected in unstaged diff: %s", pat))
			}
		}
	}

	// 4. critical config file change detection — check uncommitted changes + show diff summary
	criticalFiles := []string{
		"projects/nginx/nginx.conf",
		"projects/nginx/portal.html",
	}
	gitStatus, err := exec.Command("git", "-C", workspace, "status", "--porcelain").Output()
	if err == nil {
		statusStr := string(gitStatus)
		for _, f := range criticalFiles {
			if strings.Contains(statusStr, f) {
				// get diff summary (up to 10 lines)
				diffOut, diffErr := exec.Command("git", "-C", workspace, "diff", "--unified=0", f).Output()
				diffSummary := ""
				if diffErr == nil && len(diffOut) > 0 {
					lines := strings.Split(string(diffOut), "\n")
					limit := 10
					if len(lines) < limit {
						limit = len(lines)
					}
					diffSummary = "\n```\n" + strings.Join(lines[:limit], "\n") + "\n```"
				}
				warnings = append(warnings, fmt.Sprintf("⚙️ critical config file has uncommitted changes: %s%s", f, diffSummary))
			}
		}
	}

	// 5. Docker service config integrity — check docker-compose.yml not deleted
	projectsDir := filepath.Join(workspace, "projects")
	if entries, err := os.ReadDir(projectsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			composePath := filepath.Join(projectsDir, e.Name(), "docker-compose.yml")
			if _, err := os.Stat(composePath); err == nil {
				// compose file exists, check size
				info, _ := os.Stat(composePath)
				if info != nil && info.Size() == 0 {
					warnings = append(warnings, fmt.Sprintf("⚙️ docker-compose.yml emptied: projects/%s/", e.Name()))
				}
			}
		}
	}

	// summary
	if len(warnings) == 0 {
		fmt.Fprint(os.Stderr, "["+appName+"] safety check passed ✓\n")
		return
	}

	// issues found: output to stderr + send Telegram alert
	fmt.Fprintf(os.Stderr, "["+appName+"] safety check found %d issues:\n", len(warnings))
	var report strings.Builder
	report.WriteString(fmt.Sprintf("🛡️ *Safety Check — %s*\n\n", mode))
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  %s\n", w)
		report.WriteString(w + "\n")
	}
	report.WriteString(fmt.Sprintf("\n_checked at: %s_", time.Now().Format("2006-01-02 15:04")))
	if err := trySendTelegram(report.String()); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] safety check alert send failed: %v\n", err)
	}
}

// autoCleanTmp cleans up session temp directories older than 24 hours
func autoCleanTmp() {
	cutoff := time.Now().Add(-24 * time.Hour)
	tmpDirs := findAppTmpDirs()
	cleaned := 0
	for _, d := range tmpDirs {
		// don't clean current session's directory
		if d == sessionDir {
			continue
		}
		info, err := os.Stat(d)
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			continue
		}
		cleaned++
	}
	if cleaned > 0 {
		fmt.Fprintf(os.Stderr, "["+appName+"] auto-cleaned %d old temp directories\n", cleaned)
	}
}

// deliverReport reads the report file written by claude and sends it via Telegram
func deliverReport(mode string) {
	data, err := os.ReadFile(sessionTmp("report.txt"))
	if err != nil {
		fmt.Fprint(os.Stderr, "["+appName+"] no report file, skipping delivery\n")
		return
	}

	report := strings.TrimSpace(string(data))
	if report == "" {
		return
	}

	// add mode prefix
	prefix := map[string]string{
		"cron":      "📓",
		"heartbeat": "💓",
	}
	emoji := prefix[mode]
	if emoji == "" {
		emoji = "📋"
	}
	msg := fmt.Sprintf("%s *%s report*\n\n%s", emoji, mode, report)

	if err := trySendTelegram(msg); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] report send failed: %v\n", err)
		return
	}
	fmt.Fprint(os.Stderr, "["+appName+"] report sent to Telegram\n")
}
