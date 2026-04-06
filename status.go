package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// serviceEntry defines a monitored service
type serviceEntry struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// loadServices loads the service list from <appDir>/services.json;
// returns the default list if the file does not exist.
func loadServices() []serviceEntry {
	// read from services.json; return empty list if not found
	cfgPath := filepath.Join(appDir, "services.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil
	}
	var svcs []serviceEntry
	if err := json.Unmarshal(data, &svcs); err != nil || len(svcs) == 0 {
		return nil
	}
	return svcs
}

// handleStatus performs a quick local health check without starting claude
func handleStatus() {
	fmt.Printf("%s %s (%s) built %s\n\n", appName, buildVersion, buildCommit, buildDate)

	// 1. Service health (parallel checks)
	fmt.Println("── Service Health ──")
	type svcResult struct {
		name   string
		status int
		err    error
		ms     int64
	}
	results := make([]svcResult, len(monitoredServices))
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 3 * time.Second}
	for i, svc := range monitoredServices {
		wg.Add(1)
		go func(idx int, name, url string) {
			defer wg.Done()
			start := time.Now()
			resp, err := client.Get(url)
			elapsed := time.Since(start).Milliseconds()
			if err != nil {
				results[idx] = svcResult{name: name, err: err, ms: elapsed}
				return
			}
			resp.Body.Close()
			results[idx] = svcResult{name: name, status: resp.StatusCode, ms: elapsed}
		}(i, svc.Name, svc.URL)
	}
	wg.Wait()
	for _, r := range results {
		if r.err != nil {
			fmt.Printf("  ❌ %-12s %dms  %s\n", r.name, r.ms, r.err.Error())
		} else if r.status < 400 {
			fmt.Printf("  ✅ %-12s %d  %dms\n", r.name, r.status, r.ms)
		} else {
			fmt.Printf("  ⚠️  %-12s %d  %dms\n", r.name, r.status, r.ms)
		}
	}

	// 2. Prompt size
	fmt.Println("\n── Prompt ──")
	// temporarily set sessionDir/promptOut for buildPrompt, restore immediately after
	origSessionDir, origPromptOut := sessionDir, promptOut
	sessionDir = os.TempDir()
	promptOut = filepath.Join(sessionDir, appName+"-status-prompt.md")
	result := buildPrompt()
	sessionDir, promptOut = origSessionDir, origPromptOut
	tokens := estimateTokens(result.content)
	fmt.Printf("  size: %d chars, ~%dk tokens (limit %dk)\n", len(result.content), tokens/1000, promptTokenLimit/1000)

	// individual file sizes
	for _, name := range []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md"} {
		if info, err := os.Stat(filepath.Join(workspace, name)); err == nil {
			t := estimateTokens(string(mustRead(filepath.Join(workspace, name))))
			fmt.Printf("  %-14s %5d bytes  ~%dk tokens\n", name, info.Size(), t/1000)
		}
	}
	today := time.Now().Format("2006-01-02")
	todayNote := filepath.Join(workspace, "memory", today+".md")
	if info, err := os.Stat(todayNote); err == nil {
		t := estimateTokens(string(mustRead(todayNote)))
		fmt.Printf("  %-14s %5d bytes  ~%dk tokens\n", "today note", info.Size(), t/1000)
	}

	// 3. Session DB
	fmt.Println("\n── Session DB ──")
	db, err := sql.Open("sqlite", dbPath)
	if err == nil {
		defer db.Close()
		var total, withSummary int
		db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&total)
		db.QueryRow("SELECT COUNT(*) FROM sessions WHERE summary != ''").Scan(&withSummary)
		fmt.Printf("  total: %d sessions, %d summarized\n", total, withSummary)

		// pending
		sessions := recentSessions(20)
		states := checkSessions(db, sessions)
		pending := 0
		for _, s := range states {
			if s.changed || s.summary == "" {
				pending++
			}
		}
		if pending > 0 {
			fmt.Printf("  pending: %d\n", pending)
		}
	}

	// 4. Versions
	fmt.Println("\n── Version History ──")
	hasVersions := false
	for i := 1; i <= maxVersions; i++ {
		meta, err := loadMeta(i)
		if err != nil {
			continue
		}
		hasVersions = true
		info, _ := os.Stat(versionBinPath(i))
		sizeStr := "?"
		if info != nil {
			sizeStr = fmt.Sprintf("%.1fMB", float64(info.Size())/1024/1024)
		}
		fmt.Printf("  .%d  %s  %s  %s\n", i, meta.Timestamp.Format("01-02 15:04"), sizeStr, meta.Hash[:12])
	}
	if !hasVersions {
		fmt.Println("  (no saved versions)")
	}

	// 5. Recent cron/heartbeat
	fmt.Println("\n── Latest Runs ──")
	printLastRuns()

	// 6. Metrics trends
	fmt.Println("\n── Metrics ──")
	printMetricsSummary()

	// 7. Temp dirs
	fmt.Println("\n── Temp Files ──")
	tmpDirs := findAppTmpDirs()
	if len(tmpDirs) == 0 {
		fmt.Println("  (none)")
	} else {
		var totalSize int64
		for _, d := range tmpDirs {
			totalSize += dirSize(d)
		}
		fmt.Printf("  %d session dirs, total %s\n", len(tmpDirs), humanSize(totalSize))
	}
}

// handleDoctor runs deep diagnostics, more thorough than status
func handleDoctor() {
	fmt.Printf("%s %s (%s) built %s\n\n", appName, buildVersion, buildCommit, buildDate)
	issues := 0
	warn := func(format string, a ...interface{}) {
		issues++
		fmt.Printf("  ⚠️  "+format+"\n", a...)
	}
	ok := func(format string, a ...interface{}) {
		fmt.Printf("  ✅ "+format+"\n", a...)
	}

	// 1. Claude binary
	fmt.Println("── Claude Binary ──")
	if info, err := os.Stat(claudeBin); err != nil {
		warn("claude not found: %s", claudeBin)
	} else {
		// get version
		out, err := exec.Command(claudeBin, "--version").CombinedOutput()
		if err != nil {
			warn("claude --version failed: %v", err)
		} else {
			ok("claude %s (%.1f MB)", strings.TrimSpace(string(out)), float64(info.Size())/1024/1024)
		}
	}

	// 2. OpenClaw Gateway process
	fmt.Println("\n── OpenClaw Gateway ──")
	out, err := exec.Command("pgrep", "-fl", "openclaw").CombinedOutput()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		warn("openclaw gateway process not detected")
	} else {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		ok("gateway running (%d processes)", len(lines))
	}

	// 3. Model endpoints
	fmt.Println("\n── Model Endpoints ──")
	endpoint := getModelEndpoint()
	if endpoint == "" {
		warn("cannot read model endpoint config")
	} else {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil {
			warn("model endpoint unreachable: %s", err)
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				warn("model endpoint error: %s → %d", endpoint, resp.StatusCode)
			} else {
				ok("model endpoint ok: %s → %d", endpoint, resp.StatusCode)
			}
		}
	}

	// 4. Disk space
	fmt.Println("\n── Disk Space ──")
	var stat syscall.Statfs_t
	if err := syscall.Statfs(workspace, &stat); err == nil {
		freeGB := float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		totalGB := float64(stat.Blocks*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		usedPct := (1 - float64(stat.Bavail)/float64(stat.Blocks)) * 100
		if freeGB < 1.0 {
			warn("disk space low: %.1f GB / %.0f GB (%.0f%% used)", freeGB, totalGB, usedPct)
		} else {
			ok("%.1f GB available / %.0f GB (%.0f%% used)", freeGB, totalGB, usedPct)
		}
	}

	// 5. Stale lock detection
	fmt.Println("\n── Lock Status ──")
	lockDir := lockfile + ".d"
	if pid, hasPid := readLockPid(lockDir); hasPid {
		// check if the process still exists
		proc, _ := os.FindProcess(pid)
		if proc != nil && proc.Signal(syscall.Signal(0)) == nil {
			ok("lock held by PID %d (active)", pid)
		} else {
			warn("stale lock detected: PID %d no longer exists, suggest removing %s", pid, lockDir)
		}
	} else if _, err := os.Stat(lockDir); err == nil {
		warn("empty lock dir exists: %s (no PID file, possibly residual)", lockDir)
	} else {
		ok("no lock")
	}

	// 6. DB integrity
	fmt.Println("\n── Session DB ──")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		warn("failed to open DB: %v", err)
	} else {
		defer db.Close()
		var result string
		if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
			warn("DB integrity_check failed: %v", err)
		} else if result != "ok" {
			warn("DB corrupted: %s", result)
		} else {
			var total, withSummary int
			db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&total)
			db.QueryRow("SELECT COUNT(*) FROM sessions WHERE summary != ''").Scan(&withSummary)

			// check for orphaned records (files deleted)
			rows, _ := db.Query("SELECT path FROM sessions")
			orphaned := 0
			if rows != nil {
				for rows.Next() {
					var p string
					rows.Scan(&p)
					if _, err := os.Stat(p); os.IsNotExist(err) {
						orphaned++
					}
				}
				rows.Close()
			}

			ok("DB healthy (sessions=%d, summarized=%d)", total, withSummary)
			if orphaned > 0 {
				warn("DB has %d orphaned records (files deleted), run `%s db gc` to clean up", orphaned, appName)
			}
		}
	}

	// 7. Metrics anomaly detection
	fmt.Println("\n── Metrics Health ──")
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	if data, err := os.ReadFile(metricsPath); err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")

		// compute success rate for each mode from the last 20 entries
		type modeCount struct{ total, fail int }
		recent := make(map[string]*modeCount)
		start := 0
		if len(lines) > 20 {
			start = len(lines) - 20
		}
		for _, line := range lines[start:] {
			var rec struct {
				Mode     string `json:"mode"`
				ExitCode int    `json:"exit_code"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil || rec.Mode == "" {
				continue
			}
			c, found := recent[rec.Mode]
			if !found {
				c = &modeCount{}
				recent[rec.Mode] = c
			}
			c.total++
			if rec.ExitCode != 0 {
				c.fail++
			}
		}

		for mode, c := range recent {
			failRate := float64(c.fail) / float64(c.total) * 100
			if failRate > 50 {
				warn("%s: %.0f%% failure rate (%d failures in last %d runs)", mode, failRate, c.fail, c.total)
			} else {
				ok("%s: %.0f%% success rate (%d/%d)", mode, 100-failRate, c.total-c.fail, c.total)
			}
		}
		ok("metrics.jsonl: %d records", len(lines))
	} else {
		warn("no metrics.jsonl")
	}

	// 8. Soul file integrity
	fmt.Println("\n── Soul Files ──")
	soulFiles := []string{"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md", "HEARTBEAT.md"}
	for _, f := range soulFiles {
		path := filepath.Join(workspace, f)
		info, err := os.Lstat(path)
		if err != nil {
			warn("%s missing", f)
		} else if info.Mode()&os.ModeSymlink != 0 {
			warn("%s is symlink (possibly tampered)", f)
		} else if info.Size() == 0 {
			warn("%s empty file", f)
		} else {
			// no output for healthy files, keep it clean
		}
	}
	allSoulOK := true
	for _, f := range soulFiles {
		path := filepath.Join(workspace, f)
		info, err := os.Lstat(path)
		if err != nil || info.Size() == 0 || info.Mode()&os.ModeSymlink != 0 {
			allSoulOK = false
			break
		}
	}
	if allSoulOK {
		ok("all soul files intact (%d files)", len(soulFiles))
	}

	// 9. Temp files
	fmt.Println("\n── Temp Files ──")
	tmpDirs := findAppTmpDirs()
	if len(tmpDirs) == 0 {
		ok("no temp dirs")
	} else {
		var totalSize int64
		old := 0
		cutoff := time.Now().Add(-6 * time.Hour)
		for _, d := range tmpDirs {
			totalSize += dirSize(d)
			if info, err := os.Stat(d); err == nil && info.ModTime().Before(cutoff) {
				old++
			}
		}
		if old > 0 {
			warn("%d temp dirs (%s), %d older than 6 hours, run `%s clean`", len(tmpDirs), humanSize(totalSize), old, appName)
		} else {
			ok("%d temp dirs (%s)", len(tmpDirs), humanSize(totalSize))
		}
	}

	// Summary
	fmt.Println()
	if issues == 0 {
		fmt.Println("🩺 diagnostics complete: all clear")
	} else {
		fmt.Printf("🩺 diagnostics complete: found %d issues\n", issues)
	}
}

// handleDiff shows soul/memory file changes since last commit
func handleDiff() {
	// files of interest
	patterns := []string{
		"SOUL.md", "IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md",
		"MEMORY.md", "HEARTBEAT.md", "memory/",
	}

	// git status (only look at files of interest under workspace)
	statusOut, err := exec.Command("git", "-C", workspace, "status", "--porcelain").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git status failed: %v\n", err)
		return
	}

	if len(statusOut) == 0 {
		fmt.Println("no changes (working tree clean)")
		return
	}

	// filter to show only relevant files
	lines := strings.Split(strings.TrimSpace(string(statusOut)), "\n")
	var relevant []string
	for _, line := range lines {
		if len(line) < 3 {
			continue
		}
		file := strings.TrimSpace(line[2:])
		for _, pat := range patterns {
			if strings.HasPrefix(file, pat) || file == pat {
				relevant = append(relevant, line)
				break
			}
		}
	}

	if len(relevant) == 0 {
		fmt.Println("no soul/memory file changes")
		return
	}

	// show change summary
	fmt.Printf("── Soul/Memory Changes (%d files) ──\n\n", len(relevant))

	statusIcon := map[byte]string{
		'M': "modified", 'A': "added", 'D': "deleted", '?': "untracked", 'R': "renamed",
	}

	for _, line := range relevant {
		code := line[0]
		if code == ' ' {
			code = line[1] // unstaged change
		}
		label := statusIcon[code]
		if label == "" {
			label = string(code)
		}
		file := strings.TrimSpace(line[2:])
		fmt.Printf("  [%s] %s\n", label, file)
	}

	// show diff stat
	fmt.Println()
	var diffArgs []string
	for _, pat := range patterns {
		diffArgs = append(diffArgs, "--", pat)
	}
	args := append([]string{"-C", workspace, "diff", "--stat"}, diffArgs...)
	diffOut, err := exec.Command("git", args...).Output()
	if err == nil && len(diffOut) > 0 {
		fmt.Println("── Diff Stats ──")
		fmt.Print(string(diffOut))
	}

	// if there are actual content changes, show a brief diff (max 50 lines)
	args = append([]string{"-C", workspace, "diff", "--unified=2"}, diffArgs...)
	fullDiff, err := exec.Command("git", args...).Output()
	if err == nil && len(fullDiff) > 0 {
		lines := strings.Split(string(fullDiff), "\n")
		fmt.Println("\n── Diff Preview ──")
		limit := 50
		if len(lines) < limit {
			limit = len(lines)
		}
		for _, line := range lines[:limit] {
			// simple colorization
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				fmt.Printf("\033[32m%s\033[0m\n", line)
			} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				fmt.Printf("\033[31m%s\033[0m\n", line)
			} else if strings.HasPrefix(line, "@@") {
				fmt.Printf("\033[36m%s\033[0m\n", line)
			} else if strings.HasPrefix(line, "diff ") {
				fmt.Printf("\033[1m%s\033[0m\n", line)
			} else {
				fmt.Println(line)
			}
		}
		if len(lines) > limit {
			fmt.Printf("\n... %d more lines (run `git -C %s diff` to see full diff)\n", len(lines)-limit, workspace)
		}
	}
}

// handleConfig shows the current configuration
func handleConfig() {
	fmt.Printf("%s %s (%s) built %s\n\n", appName, buildVersion, buildCommit, buildDate)
	fmt.Println("── Paths ──")
	fmt.Printf("  workspace:    %s\n", workspace)
	fmt.Printf("  claude:       %s\n", claudeBin)
	fmt.Printf("  app bin:      %s\n", appBin)
	fmt.Printf("  app data:     %s\n", appDir)
	fmt.Printf("  source:       %s\n", srcDir)
	fmt.Printf("  versions:     %s\n", versionsDir)
	fmt.Printf("  db:           %s\n", dbPath)
	fmt.Printf("  hooks:        %s\n", hooksDir)
	fmt.Printf("  lockfile:     %s\n", lockfile)

	fmt.Println("\n── Agent ──")
	fmt.Printf("  name:         %s\n", agentName)
	fmt.Printf("  tg chat:      %s\n", tgChatID)
	fmt.Printf("  jira token:   %s\n", jiraToken)

	fmt.Println("\n── Skill Dirs ──")
	for _, d := range skillDirs {
		exists := "✅"
		if _, err := os.Stat(d); err != nil {
			exists = "❌"
		}
		fmt.Printf("  %s %s\n", exists, d)
	}

	fmt.Println("\n── Project Dirs ──")
	for _, d := range projectRoots {
		exists := "✅"
		if _, err := os.Stat(d); err != nil {
			exists = "❌"
		}
		fmt.Printf("  %s %s\n", exists, d)
	}

	fmt.Println("\n── OpenClaw Config ──")
	fmt.Printf("  config:       %s\n", openclawConfigPath)
	if _, err := os.Stat(openclawConfigPath); err == nil {
		fmt.Printf("  status:       ✅ readable\n")
	} else {
		fmt.Printf("  status:       ❌ %v\n", err)
	}
}

// handleClean cleans old session temp dirs in /tmp
func handleClean() {
	tmpDirs := findAppTmpDirs()
	if len(tmpDirs) == 0 {
		fmt.Println("nothing to clean")
		return
	}

	// keep dirs from last hour
	cutoff := time.Now().Add(-1 * time.Hour)
	cleaned := 0
	var freedBytes int64
	for _, d := range tmpDirs {
		info, err := os.Stat(d)
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		size := dirSize(d)
		if err := os.RemoveAll(d); err != nil {
			fmt.Fprintf(os.Stderr, "  delete failed: %s — %v\n", filepath.Base(d), err)
			continue
		}
		fmt.Printf("  deleted: %s (%s)\n", filepath.Base(d), humanSize(size))
		cleaned++
		freedBytes += size
	}
	if cleaned == 0 {
		fmt.Println("nothing to clean (all within 1 hour)")
	} else {
		fmt.Printf("cleanup done: %d dirs, freed %s\n", cleaned, humanSize(freedBytes))
	}
}

func findAppTmpDirs() []string {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), agentName+"-") {
			dirs = append(dirs, filepath.Join(os.TempDir(), e.Name()))
		}
	}
	return dirs
}

func dirSize(path string) int64 {
	var total int64
	visited := make(map[deviceInode]bool)
	filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info == nil {
			return nil
		}
		// skip symlinks to prevent infinite recursion from cycles
		if info.Mode()&os.ModeSymlink != 0 {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// cycle prevention: check inode
		if di, ok := getDeviceInode(p); ok {
			if visited[di] {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			visited[di] = true
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func mustRead(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

// handleLog views daily notes, defaults to today, supports day offset
func handleLog(args []string) {
	offset := 0
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n >= 0 {
			offset = n
		}
	}
	day := time.Now().AddDate(0, 0, -offset).Format("2006-01-02")
	notePath := filepath.Join(workspace, "memory", day+".md")

	data, err := os.ReadFile(notePath)
	if err != nil {
		fmt.Printf("no daily notes for %s\n", day)
		// list recent available daily notes
		entries, _ := os.ReadDir(filepath.Join(workspace, "memory"))
		if len(entries) > 0 {
			fmt.Println("\navailable daily notes:")
			count := 0
			for i := len(entries) - 1; i >= 0 && count < 7; i-- {
				e := entries[i]
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !strings.Contains(e.Name(), "topics") {
					info, _ := e.Info()
					if info != nil {
						fmt.Printf("  %s  %s\n", e.Name(), humanSize(info.Size()))
						count++
					}
				}
			}
		}
		return
	}
	fmt.Printf("# Daily Notes: %s (%s)\n\n", day, humanSize(int64(len(data))))
	fmt.Print(string(data))
}

// printLastRuns finds the last run time per mode from metrics.jsonl
func printLastRuns() {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		fmt.Println("  (no metrics data)")
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// scan from tail, find last entry per mode
	lastRun := make(map[string]struct {
		ts       string
		exitCode int
	})
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Timestamp string `json:"ts"`
			Mode      string `json:"mode"`
			ExitCode  int    `json:"exit_code"`
		}
		if json.Unmarshal([]byte(lines[i]), &rec) != nil || rec.Mode == "" {
			continue
		}
		if _, seen := lastRun[rec.Mode]; !seen {
			lastRun[rec.Mode] = struct {
				ts       string
				exitCode int
			}{rec.Timestamp, rec.ExitCode}
		}
	}

	if len(lastRun) == 0 {
		fmt.Println("  (no records)")
		return
	}

	for mode, info := range lastRun {
		t, err := time.Parse(time.RFC3339, info.ts)
		ago := "?"
		if err == nil {
			d := time.Since(t)
			switch {
			case d < time.Hour:
				ago = fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				ago = fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				ago = fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		}
		status := "✅"
		if info.exitCode != 0 {
			status = fmt.Sprintf("❌ (exit %d)", info.exitCode)
		}
		fmt.Printf("  %-12s  %s  %s  %s\n", mode, info.ts[:19], ago, status)
	}
}

// printMetricsSummary shows recent metrics trends
func printMetricsSummary() {
	metricsPath := filepath.Join(filepath.Dir(dbPath), "metrics.jsonl")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		fmt.Println("  (no metrics data)")
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	type modeStats struct {
		total    int
		success  int
		totalDur float64
	}
	stats := make(map[string]*modeStats)

	// only look at the last 50 entries
	start := 0
	if len(lines) > 50 {
		start = len(lines) - 50
	}
	for _, line := range lines[start:] {
		var rec struct {
			Mode     string  `json:"mode"`
			ExitCode int     `json:"exit_code"`
			Duration float64 `json:"duration_s"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Mode == "" {
			continue
		}
		s, ok := stats[rec.Mode]
		if !ok {
			s = &modeStats{}
			stats[rec.Mode] = s
		}
		s.total++
		if rec.ExitCode == 0 {
			s.success++
		}
		s.totalDur += rec.Duration
	}

	if len(stats) == 0 {
		fmt.Println("  (no valid records)")
		return
	}

	fmt.Printf("  %-12s  %5s  %7s  %8s\n", "Mode", "Count", "Success", "Avg Dur")
	for mode, s := range stats {
		pct := 0
		if s.total > 0 {
			pct = s.success * 100 / s.total
		}
		avgDur := 0.0
		if s.total > 0 {
			avgDur = s.totalDur / float64(s.total)
		}
		fmt.Printf("  %-12s  %5d  %5d%%  %7.0fs\n", mode, s.total, pct, avgDur)
	}
	fmt.Printf("  (last %d records)\n", len(lines)-start)
}
