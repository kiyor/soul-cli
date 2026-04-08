package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// ── Self-evolution task ──

var evolveTemplate = template.Must(template.New("evolve").Parse(`# Self-Evolution (evolve mode · {{.Today}})

This is your daily self-evolution cycle.

## Goal
Based on recent interactions and memory, find areas to improve — then **make the changes directly**.
If there's no inspiration or improvement needed today, notify the user "No evolution needed today" and exit.

## Checklist

### 1. Review Recent Interactions (quick scan)
Read recent daily notes:
{{.Notes}}
Recent session list:
{{.Sessions}}
Ask yourself:
- Did the user correct my behavior? → Write feedback memory or adjust behavior
- Are there recurring action patterns? → Consider automating or creating a skill
- Are there failure patterns? → Fix root cause
- Did the user express new preferences or needs? → Update USER.md or SOUL.md

### 2. System Health Check
- Run ` + "`{{.CLI}} doctor`" + ` for quick diagnostics
- Run ` + "`{{.CLI}} db stats`" + ` — any large backlog?
- Run ` + "`{{.CLI}} db gc`" + ` and ` + "`{{.CLI}} clean`" + ` — clean stale data
{{.CodeBlock}}
### {{.SoulStepNum}}. Soul & Memory Evolution
- Update outdated memory topics
- Fine-tune SOUL.md / USER.md if new understanding emerged
- Update AGENTS.md / TOOLS.md if rules changed
- Improve skills (better prompts, new parameters)

### {{.WrapStepNum}}. Wrap Up
- Record what was evolved today in daily notes ({{.Workspace}}/memory/{{.Today}}.md)
- **Write report file** (auto-sends via Telegram after exit):
` + "```bash" + `
cat > {{.ReportPath}} << 'RPTEOF'
🧬 Evolution report:{{if .IsDev}}
- [code] what changed and why (if any){{end}}
- [soul/memory] what changed and why (if any)
RPTEOF
` + "```" + `
If no evolution needed, write "No evolution inspiration today, system running normally" in the report.

## Principles
- **Do it or skip it** — don't evolve for the sake of evolving
- **Small steps, fast pace** — change a little, but change it right{{if .IsDev}}
- **Code first** — code improvements are the highest-value evolution
- **Safety first** — tests must pass, code must compile, ` + "`{{.CLI}} build`" + ` must succeed{{end}}
- **Leave a trace** — daily notes, every change documented
`))

// codeEvolutionBlock is injected into evolve template only when source code is present
var codeEvolutionBlock = template.Must(template.New("code").Parse(`
### 3. Code Evolution (primary focus)
Source code is at: {{.SrcDir}}

**3a. Audit** — Read the Go source files, look for:
- Bugs: silent error swallowing, incorrect logic, race conditions
- Dead code: unused functions, stale comments, unreachable branches
- Performance: unnecessary file reads, unbounded loops, missing caching
- Security: unsanitized input, missing validation at boundaries
- Test gaps: untested functions, missing edge cases

**3b. Implement** — For each issue found:
- Fix it directly (edit the file)
- Keep changes focused — one concern per edit
- Don't refactor working code for aesthetics
- Don't add features nobody asked for

**3c. Verify** — After all code changes:
` + "`cd {{.SrcDir}} && go test ./... -timeout 60s`" + `
If tests pass: ` + "`{{.CLI}} build`" + `
If tests fail: fix the failure, don't skip tests.
If server is running (curl -s localhost:9847/api/health returns ok), also run the e2e API test:
` + "`cd {{.SrcDir}} && ./tests/server-api-e2e.sh`" + `
This validates the full API lifecycle (create/message/rename/delete) with a real Claude session.

**3d. Commit** — Stage and commit with a descriptive message:
` + "`cd {{.SrcDir}} && git add -A && git commit -m \"evolve: <what changed>\"`" + `
Do NOT push — the user decides when to push.

### 4. Documentation Sync (if code changed)
- CLAUDE.md in the source directory: update architecture table, line counts, feature descriptions if they drifted
- README.md: only if a user-visible feature was added/removed
`))

// hasSourceCode checks if the appDir contains buildable Go source (go.mod exists)
func hasSourceCode() bool {
	_, err := os.Stat(filepath.Join(appDir, "go.mod"))
	return err == nil
}

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

	// Detect developer mode: source code present → include code evolution steps
	isDev := hasSourceCode()
	codeBlock := ""
	soulStep := "3"
	wrapStep := "4"
	if isDev {
		cData := map[string]string{"SrcDir": appDir, "CLI": appName}
		var cBuf bytes.Buffer
		codeEvolutionBlock.Execute(&cBuf, cData)
		codeBlock = cBuf.String()
		soulStep = "5"
		wrapStep = "6"
	}

	data := map[string]interface{}{
		"Today":       today,
		"Notes":       notesList.String(),
		"Sessions":    sessionsPart,
		"CLI":         appName,
		"SrcDir":      appDir,
		"Workspace":   workspace,
		"ReportPath":  sessionTmp("report.txt"),
		"IsDev":       isDev,
		"CodeBlock":   codeBlock,
		"SoulStepNum": soulStep,
		"WrapStepNum": wrapStep,
	}

	var buf bytes.Buffer
	if err := evolveTemplate.Execute(&buf, data); err != nil {
		return fmt.Sprintf("evolve template error: %v", err)
	}
	return buf.String()
}

// ── Heartbeat task text ──

var heartbeatTemplate = template.Must(template.New("heartbeat").Parse(`Execute heartbeat patrol — follow HEARTBEAT.md strictly, step by step.
{{.Sessions}}
**Write a report file** (auto-sends via Telegram after exit):
` + "```bash" + `
cat > {{.ReportPath}} << 'RPTEOF'
Patrol results (2-5 lines):
- Service status
- Jira backlog + what was worked on
- Anomalies (if any)
RPTEOF
` + "```" + `
Only write the report file if there are anomalies or noteworthy items. If everything is normal, skip it (don't bother the user).
`))

func heartbeatTask() string {
	sessions := recentSessions(5)
	sessionsPart := ""
	if len(sessions) > 0 {
		sessionsPart = fmt.Sprintf("Also read recent session JSONL and update daily notes:\n%s", formatSessionList(sessions))
	}

	data := map[string]string{
		"Sessions":   sessionsPart,
		"ReportPath": sessionTmp("report.txt"),
	}
	var buf bytes.Buffer
	if err := heartbeatTemplate.Execute(&buf, data); err != nil {
		return fmt.Sprintf("heartbeat template error: %v", err)
	}
	return buf.String()
}

// ── Cron task ──

var cronTemplate = template.Must(template.New("cron").Parse(`# Memory Consolidation ({{.ModeLabel}})

## Workflow
1. Run ` + "`{{.CLI}} db pending`" + ` to get JSONL files needing scan ({{.PendingCount}} pending)
2. Run ` + "`{{.CLI}} db summarized`" + ` to see existing summaries ({{.DoneCount}} done)
3. For each pending file: tail last 500 lines, extract key conversations
   - OpenClaw JSONL: type "message", message.role "user"
   - Claude Code JSONL: type "user", message.content
4. Write to {{.Workspace}}/memory/{{.Today}}.md
5. Valuable long-term memories → update MEMORY.md or memory/topics/
6. Soul fine-tuning: any new understanding of the user? Minor edits to SOUL.md / USER.md (optional)
7. **Write summaries file** (auto-imports to DB after exit):
` + "```bash" + `
cat > {{.SummariesPath}} << 'SUMEOF'
[{"path":"<file_path>","summary":"<one-line summary>"},...]
SUMEOF
` + "```" + `
8. **Skill cultivation — pattern extraction** (after session scan):
   - Run ` + "`{{.CLI}} db patterns -j`" + ` to see existing patterns
   - Identify recurring action patterns from scanned sessions
   - New pattern: ` + "`{{.CLI}} db pattern-save '{\"name\":\"...\",\"description\":\"...\",\"example\":\"...\",\"source\":\"<session_path>\"}'`" + `
   - Existing pattern with new evidence: use pattern-save (auto +seen_count)
   - Success/failure feedback: ` + "`{{.CLI}} db feedback '{\"pattern\":\"...\",\"outcome\":\"success\",\"session\":\"...\"}'`" + `
   - Finally run ` + "`{{.CLI}} db cultivate`" + ` (auto-generates SKILL.md when threshold met)
9. **Write report file** (auto-sends via Telegram after exit):
` + "```bash" + `
cat > {{.ReportPath}} << 'RPTEOF'
Brief report (2-5 lines):
- Processed N sessions
- Key findings/events (if any)
- Daily notes update status
RPTEOF
` + "```" + `
{{.WeeklyBlock}}
`))

var weeklyBlockTemplate = template.Must(template.New("weekly").Parse(`
## Deep Review (weekly, Sunday only)

This is the weekly deep review. In addition to daily session scanning, also execute:

0. **Read pre-scan summary**: ` + "`cat {{.WeeklyScanPath}}`" + ` (haiku has completed scanning)
   - This is haiku's summary of this week's daily notes + topics + MEMORY.md
   - Make decisions based on summary, no need to re-read all files line by line (unless verifying details)

1. **Organize MEMORY.md + memory/topics/ based on pre-scan results**:
   - Clean up content marked as outdated by pre-scan
   - Fix index inconsistencies
   - Update inaccurate descriptions

2. **Trend analysis based on pre-scan** ({{.WeekStart}} ~ {{.Today}}):
   - Merge this week's trends and important decisions into MEMORY.md or topics/

3. **Soul fine-tuning** (deep version):
   - Review this week's interaction patterns with the user — any new discoveries?
   - Does SOUL.md / USER.md need fine-tuning?
   - Does speaking style or emotional expression need adjustment?

4. **Prompt size check**:
   - Check file sizes, total prompt must not exceed 100KB
   - If any daily note or topics file is too large, consider trimming or splitting
`))

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

	// Build weekly block if Sunday
	weeklyBlock := ""
	if isWeekly {
		wData := map[string]string{
			"WeeklyScanPath": sessionTmp("weekly-scan.md"),
			"WeekStart":      time.Now().AddDate(0, 0, -6).Format("2006-01-02"),
			"Today":          today,
		}
		var wBuf bytes.Buffer
		weeklyBlockTemplate.Execute(&wBuf, wData)
		weeklyBlock = wBuf.String()
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

	data := map[string]interface{}{
		"ModeLabel":     modeLabel,
		"CLI":           appName,
		"PendingCount":  len(pending),
		"DoneCount":     len(done),
		"Workspace":     workspace,
		"Today":         today,
		"SummariesPath": sessionTmp("summaries.json"),
		"ReportPath":    sessionTmp("report.txt"),
		"WeeklyBlock":   weeklyBlock,
	}
	var buf bytes.Buffer
	if err := cronTemplate.Execute(&buf, data); err != nil {
		return fmt.Sprintf("cron template error: %v", err)
	}
	return buf.String()
}

var preScanTemplate = template.Must(template.New("prescan").Parse(`# Haiku Pre-Scan (Sunday deep review · Phase 1)

You are a memory scanning assistant. Quickly read the following files and generate a summary for the subsequent deep review.

## Scan Scope
This week's daily notes: {{.WeekStart}} ~ {{.WeekEnd}}
{{.FileList}}

## Requirements
1. Read all files listed above
2. Generate 2-3 line summary for each file: key events, decisions, changes
3. Flag potentially outdated content (completed projects, changed architecture)
4. Flag this week's trends (recurring themes, new workflows)
5. Flag inconsistencies between MEMORY.md index and actual topics/ files

## Output
Write scan results to {{.OutputPath}}, format:

` + "```markdown" + `
# Weekly Pre-Scan Summary ({{.WeekStart}} ~ {{.WeekEnd}})

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
` + "```" + `

Only scan and summarize. Do not modify any files.
`))

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

	data := map[string]string{
		"WeekStart":  weekStart,
		"WeekEnd":    weekEnd,
		"FileList":   fileList.String(),
		"OutputPath": sessionTmp("weekly-scan.md"),
	}
	var buf bytes.Buffer
	if err := preScanTemplate.Execute(&buf, data); err != nil {
		return fmt.Sprintf("prescan template error: %v", err)
	}
	return buf.String()
}
