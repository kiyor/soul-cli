package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── Self-evolution task ──

func evolveTask() string {
	today := time.Now().Format("2006-01-02")

	// Collect recent 3 days' daily notes paths
	var recentNotes []string
	for i := 0; i < 3; i++ {
		day := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		p := filepath.Join(workspace, "memory", day+".md")
		if _, err := os.Stat(p); err == nil {
			recentNotes = append(recentNotes, p)
		}
	}
	var notesList strings.Builder
	for _, n := range recentNotes {
		rel, _ := filepath.Rel(workspace, n)
		notesList.WriteString(fmt.Sprintf("- %s\n", rel))
	}

	// Collect recent 10 session summaries
	sessions := recentSessions(10)
	sessionsPart := ""
	if len(sessions) > 0 {
		sessionsPart = formatSessionList(sessions)
	}

	return fmt.Sprintf(`# Self-Evolution (evolve mode · %s)

This is your daily self-evolution cycle.

## Goal
Based on recent interactions and memory, find areas to improve yourself, then **make the changes directly**.
If there's no inspiration or improvement needed today, notify the user "No evolution needed today" and exit.

## Checklist

### 1. Review Recent Interactions (quick scan)
Read recent daily notes:
%s
Recent session list:
%s
Ask yourself:
- Did the user correct my behavior? → Write feedback memory or adjust behavior
- Are there recurring action patterns? → Consider automating or creating a skill
- Are there failure patterns? → Fix root cause
- Did the user express new preferences or needs? → Update USER.md or SOUL.md

### 2. Check System Health
- Any TODOs or known issues in ` + appName + ` CLI? Run ` + "`` + appName + ` doctor`" + `
- Memory system: ` + "`` + appName + ` db stats`" + `, any large backlog?
- Skills: any skills that need updating?
- AGENTS.md / TOOLS.md: any outdated rules?

### 3. Execute Evolution (selective, only do what's valuable)
Possible improvement directions (not limited to):
- Fix ` + appName + ` CLI bugs or add small features
- Update outdated memory topics
- Fine-tune SOUL.md / USER.md (based on new understanding)
- Improve skills (better prompts, new parameters)
- Write new automation hooks
- Clean up tech debt

### 4. Wrap Up
- If code was changed: ` + "`cd %s && go test ./... && ` + appName + ` build`" + `
- If config/memory was changed: verify format is correct
- Record what was evolved today in daily notes

### 5. Notify User
**Write a report file** (auto-sends via Telegram after exit):
` + "```bash" + `
cat > %s << 'RPTEOF'
Evolution report (2-5 lines):
- What was done
- Why
- Result
RPTEOF
` + "```" + `
If no evolution needed, write "No evolution inspiration today, system running normally" in the report.

## Principles
- **Do it or skip it** — don't evolve for the sake of evolving
- **Small steps, fast pace** — change a little, but change it right
- **Safety first** — test after changes, code must compile
- **Leave a trace** — record what changed and why in daily notes
`, today, notesList.String(), sessionsPart, appDir, sessionTmp("report.txt"))
}

// ── Heartbeat task text ──

func heartbeatTask() string {
	sessions := recentSessions(5)
	sessionsPart := ""
	if len(sessions) > 0 {
		sessionsPart = fmt.Sprintf("3) Read recent session JSONL and update daily notes:\n%s", formatSessionList(sessions))
	}

	return fmt.Sprintf(`Execute heartbeat patrol:
1) jira-cli checkin main, handle urgent tickets
2) Check key services (curl health endpoints or index pages for monitored services)
%s
4) **Write a report file** (auto-sends via Telegram after exit):
`+"```bash"+`
cat > %s << 'RPTEOF'
Patrol results (2-5 lines):
- Service status
- Jira backlog
- Anomalies (if any)
RPTEOF
`+"```"+`
Only write the report file if there are anomalies or noteworthy items. If everything is normal, skip it (don't bother the user).`, sessionsPart, sessionTmp("report.txt"))
}

// ── Cron task ──

func cronTask() string {
	today := time.Now().Format("2006-01-02")
	isWeekly := time.Now().Weekday() == time.Sunday

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] %v\n", err)
		return fmt.Sprintf("Memory consolidation mode (%s)\n\nDB open failed, skipping session scan.", today)
	}
	sessions := recentSessions(20)
	states := checkSessions(db, sessions)
	db.Close()

	var pending, done []sessionState
	for _, s := range states {
		if s.changed || s.summary == "" {
			pending = append(pending, s)
		} else {
			done = append(done, s)
		}
	}

	// Sunday: append deep review instructions
	weeklyBlock := ""
	if isWeekly {
		weeklyBlock = fmt.Sprintf(`

## Deep Review (weekly, Sunday only)

This is the weekly deep review. In addition to daily session scanning, also execute:

0. **Read pre-scan summary**: `+"`cat "+sessionTmp("weekly-scan.md")+"`"+` (haiku has completed scanning)
   - This is haiku's summary of this week's daily notes + topics + MEMORY.md
   - Make decisions based on summary, no need to re-read all files line by line (unless verifying details)

1. **Organize MEMORY.md + memory/topics/ based on pre-scan results**:
   - Clean up content marked as outdated by pre-scan
   - Fix index inconsistencies
   - Update inaccurate descriptions

2. **Trend analysis based on pre-scan** (%s ~ %s):
   - Merge this week's trends and important decisions into MEMORY.md or topics/

3. **Soul fine-tuning** (deep version):
   - Review this week's interaction patterns with the user — any new discoveries?
   - Does SOUL.md / USER.md need fine-tuning?
   - Does speaking style or emotional expression need adjustment?

4. **Prompt size check**:
   - Check file sizes, total prompt must not exceed 100KB
   - If any daily note or topics file is too large, consider trimming or splitting
`, time.Now().AddDate(0, 0, -6).Format("2006-01-02"), today)
	}

	if len(pending) == 0 {
		var b strings.Builder
		if isWeekly {
			fmt.Fprintf(&b, "# Memory Consolidation (cron mode · Sunday deep review)\n\n")
		} else {
			fmt.Fprintf(&b, "# Memory Consolidation (cron mode)\n\n")
		}
		fmt.Fprintf(&b, "All recent sessions already have summaries, no scanning needed.\n\n")
		for _, s := range done {
			fmt.Fprintf(&b, "- %s → %s\n", filepath.Base(s.path), s.summary)
		}
		fmt.Fprintf(&b, "\nIf daily notes need updating (based on summaries above), make minor adjustments.\nTarget file: %s/memory/%s.md", workspace, today)
		b.WriteString(weeklyBlock)
		return b.String()
	}

	modeLabel := "cron mode"
	if isWeekly {
		modeLabel = "cron mode · Sunday deep review"
	}

	return fmt.Sprintf(`# Memory Consolidation (%s)

## Workflow
1. Run `+"`` + appName + ` db pending`"+` to get JSONL files needing scan (%d pending)
2. Run `+"`` + appName + ` db summarized`"+` to see existing summaries (%d done)
3. For each pending file: tail last 500 lines, extract key conversations
   - OpenClaw JSONL: type "message", message.role "user"
   - Claude Code JSONL: type "user", message.content
4. Write to %s/memory/%s.md
5. Valuable long-term memories → update MEMORY.md or memory/topics/
6. Soul fine-tuning: any new understanding of the user? Minor edits to SOUL.md / USER.md (optional)
7. **Write summaries file** (auto-imports to DB after exit):
`+"```bash"+`
cat > %s << 'SUMEOF'
[{"path":"<file_path>","summary":"<one-line summary>"},...]
SUMEOF
`+"```"+`
8. **Skill cultivation — pattern extraction** (after session scan):
   - Run ` + "`` + appName + ` db patterns -j`" + ` to see existing patterns
   - Identify recurring action patterns from scanned sessions
   - New pattern: ` + "`` + appName + ` db pattern-save '{\"name\":\"...\",\"description\":\"...\",\"example\":\"...\",\"source\":\"<session_path>\"}'`" + `
   - Existing pattern with new evidence: use pattern-save (auto +seen_count)
   - Success/failure feedback: ` + "`` + appName + ` db feedback '{\"pattern\":\"...\",\"outcome\":\"success\",\"session\":\"...\"}'`" + `
   - Finally run ` + "`` + appName + ` db cultivate`" + ` (auto-generates SKILL.md when threshold met)
9. **Write report file** (auto-sends via Telegram after exit):
`+"```bash"+`
cat > %s << 'RPTEOF'
Brief report (2-5 lines):
- Processed N sessions
- Key findings/events (if any)
- Daily notes update status
RPTEOF
`+"```"+`
%s`, modeLabel, len(pending), len(done), workspace, today, sessionTmp("summaries.json"), sessionTmp("report.txt"), weeklyBlock)
}

func weeklyPreScan() string {
	today := time.Now()
	weekStart := today.AddDate(0, 0, -6).Format("2006-01-02")
	weekEnd := today.Format("2006-01-02")

	// List 7 days of daily notes and topics files
	var files []string
	for i := 0; i < 7; i++ {
		day := today.AddDate(0, 0, -i).Format("2006-01-02")
		p := filepath.Join(workspace, "memory", day+".md")
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	topicsDir := filepath.Join(workspace, "memory", "topics")
	if entries, err := os.ReadDir(topicsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				files = append(files, filepath.Join(topicsDir, e.Name()))
			}
		}
	}
	memoryMd := filepath.Join(workspace, "MEMORY.md")
	if _, err := os.Stat(memoryMd); err == nil {
		files = append(files, memoryMd)
	}

	var fileList strings.Builder
	for _, f := range files {
		rel, _ := filepath.Rel(workspace, f)
		info, _ := os.Stat(f)
		if info != nil {
			fmt.Fprintf(&fileList, "- %s (%dKB)\n", rel, info.Size()/1024)
		}
	}

	return fmt.Sprintf(`# Haiku Pre-Scan (Sunday deep review · Phase 1)

You are a memory scanning assistant. Quickly read the following files and generate a summary for the subsequent deep review.

## Scan Scope
This week's daily notes: %s ~ %s
%s

## Requirements
1. Read all files listed above
2. Generate 2-3 line summary for each file: key events, decisions, changes
3. Flag potentially outdated content (completed projects, changed architecture)
4. Flag this week's trends (recurring themes, new workflows)
5. Flag inconsistencies between MEMORY.md index and actual topics/ files

## Output
Write scan results to %s, format:

`+"```markdown"+`
# Weekly Pre-Scan Summary (%s ~ %s)

## Daily Notes Summary
### YYYY-MM-DD
- Key point 1
- Key point 2

## Topics File Status
### topic-name.md
- Summary / needs update?

## Potentially Outdated Content
- file:line — reason

## This Week's Trends
- Trend 1
- Trend 2

## MEMORY.md Index Consistency
- Issues (if any)
`+"```"+`

Only scan and summarize. Do not modify any files.
`, weekStart, weekEnd, fileList.String(), sessionTmp("weekly-scan.md"), weekStart, weekEnd)
}
