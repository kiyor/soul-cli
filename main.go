package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed VERSION
var embeddedVersion string

// Injected at build time: go build -ldflags "-X main.buildDate=xxx -X main.buildCommit=xxx -X main.defaultAppName=weiran"
var (
	buildVersion   = ""
	buildDate      = "unknown"
	buildCommit    = "unknown"
	defaultAppName = "" // if set via ldflags, takes priority over os.Args[0]
)

var (
	home    = os.Getenv("HOME")
	appName string // derived from os.Args[0] basename, e.g. "weiran", "soul"

	workspace string // dynamically read from openclaw.json, fallback ~/.openclaw/workspace
	agentName string // read from openclaw.json agents.list main agent name, defaults to appName
	claudeBin = filepath.Join(home, ".local", "bin", "claude")
	lockfile  string // /tmp/<appName>.lock
	dbPath    string // <appDir>/sessions.db

	// Per-session temp directory, initialized by initSessionDir()
	sessionDir string // /tmp/<agent>-0405-0924/
	promptOut  string // sessionDir/prompt.md

	jiraToken string // read from config.json or JIRA_TOKEN env var

	// Skill directories
	skillDirs []string

	// Telegram (read from config.json / openclaw.json / <APPNAME>_TG_CHAT_ID)
	tgChatID string

	// Base directory: <APPNAME>_HOME env overrides default ~/.openclaw
	appHome            string
	openclawConfigPath string

	// Version management
	versionsDir string // <appDir>/.versions/
	appBin      string // ~/go/bin/<appName>
	maxVersions = 3

	// App data directory: where sessions.db, metrics.jsonl, hooks/, .versions/ live
	appDir string // workspace/scripts/<appName>/

	// Service health check (from services.json or fallback defaults)
	monitoredServices []serviceEntry

	// Current run mode (cron/heartbeat/interactive etc.), used for metrics recording
	currentMode string

	// Extra projectRoots read from config.json
	extraProjectRoots []string
)

// initAppName derives appName from (in priority order):
//  1. ldflags: -X main.defaultAppName=weiran
//  2. AGENT_NAME env var
//  3. os.Args[0] basename
func initAppName() {
	switch {
	case defaultAppName != "":
		appName = defaultAppName
	case os.Getenv("AGENT_NAME") != "":
		appName = os.Getenv("AGENT_NAME")
	case len(os.Args) > 0:
		appName = filepath.Base(os.Args[0])
		appName = strings.TrimSuffix(appName, ".test")
	}
	if appName == "" || appName == "." {
		appName = "soul" // fallback for tests or edge cases
	}

	// Derive early vars that depend on appName
	upperName := strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
	appHome = envOrDefault(upperName+"_HOME", envOrDefault("WEIRAN_HOME", filepath.Join(home, ".openclaw")))
	openclawConfigPath = filepath.Join(appHome, "openclaw.json")
	lockfile = filepath.Join(os.TempDir(), appName+".lock")
	appBin = filepath.Join(home, "go", "bin", appName)
}

// initWorkspace reads workspace path and agent name from openclaw.json and initializes all derived paths.
// Fallback: if the config file doesn't exist or fails to parse, uses ~/.openclaw/workspace.
func initWorkspace() {
	defaultWS := filepath.Join(appHome, "workspace")
	workspace = defaultWS
	agentName = appName

	data, err := os.ReadFile(openclawConfigPath)
	if err == nil {
		var cfg struct {
			Agents struct {
				Defaults struct {
					Workspace string `json:"workspace"`
				} `json:"defaults"`
				List []struct {
					ID        string `json:"id"`
					Name      string `json:"name"`
					Default   bool   `json:"default"`
					Workspace string `json:"workspace"`
				} `json:"list"`
			} `json:"agents"`
		}
		if json.Unmarshal(data, &cfg) == nil {
			// Match agent: first try appName as agent ID, then "main", then default
			var matched bool
			for _, a := range cfg.Agents.List {
				if a.ID == appName {
					if a.Workspace != "" {
						workspace = a.Workspace
					}
					if a.Name != "" {
						agentName = a.Name
					}
					matched = true
					break
				}
			}
			if !matched {
				for _, a := range cfg.Agents.List {
					if a.ID == "main" || a.Default {
						if a.Workspace != "" {
							workspace = a.Workspace
						}
						if a.Name != "" {
							agentName = a.Name
						}
						break
					}
				}
			}
			// If matched agent has no workspace of its own, use defaults
			if workspace == defaultWS && cfg.Agents.Defaults.Workspace != "" {
				workspace = cfg.Agents.Defaults.Workspace
			}
		}
	}

	// Telegram chat ID: read from openclaw credentials (lowest priority)
	allowFromPath := filepath.Join(appHome, "credentials", "telegram-default-allowFrom.json")
	if afData, err := os.ReadFile(allowFromPath); err == nil {
		var af struct {
			AllowFrom []string `json:"allowFrom"`
		}
		if json.Unmarshal(afData, &af) == nil && len(af.AllowFrom) > 0 {
			tgChatID = af.AllowFrom[0]
		}
	}

	// Read user config from config.json (may override tgChatID/jiraToken/projectRoots)
	loadConfig()

	// App data directory: prefer workspace/scripts/<appName>/, fallback to executable dir
	appDir = filepath.Join(workspace, "scripts", appName)
	if _, err := os.Stat(appDir); err != nil {
		// try executable directory as fallback
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Dir(exe)
			if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
				appDir = candidate
			}
		}
		// ultimate fallback: create the directory
		if _, err := os.Stat(appDir); err != nil {
			os.MkdirAll(appDir, 0755)
		}
	}

	// Service monitor list: read from <appDir>/services.json, fallback to hardcoded defaults
	monitoredServices = loadServices()

	// Initialize derived paths
	dbPath = filepath.Join(appDir, "sessions.db")
	versionsDir = filepath.Join(appDir, ".versions")
	skillDirs = []string{
		filepath.Join(appHome, "skills"),
		filepath.Join(workspace, "skills"),
	}
	hooksDir = filepath.Join(appDir, "hooks")

	// projectRoots: workspace/projects is always included, extras from config.json
	projectRoots = []string{
		filepath.Join(workspace, "projects"),
		filepath.Join(workspace, "familydash"),
	}
	projectRoots = append(projectRoots, extraProjectRoots...)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadConfig reads user configuration from config.json.
// Config file is located in the app data directory.
// All fields are optional; if unset, values are read from environment variables or openclaw.json.
func loadConfig() {
	configPaths := []string{
		filepath.Join(workspace, "scripts", appName, "config.json"),
	}
	// Also check the directory containing the binary (for development)
	if exe, err := os.Executable(); err == nil {
		configPaths = append(configPaths, filepath.Join(filepath.Dir(exe), "config.json"))
	}

	var cfgData []byte
	for _, p := range configPaths {
		if d, err := os.ReadFile(p); err == nil {
			cfgData = d
			break
		}
	}

	type appConfig struct {
		JiraToken      string   `json:"jiraToken"`
		TelegramChatID string   `json:"telegramChatID"`
		ProjectRoots   []string `json:"projectRoots"`
		AgentName      string   `json:"agentName"`
	}
	var cfg appConfig
	if cfgData != nil {
		json.Unmarshal(cfgData, &cfg)
	}

	// jiraToken: config.json > JIRA_TOKEN env var
	if cfg.JiraToken != "" {
		jiraToken = cfg.JiraToken
	}
	if envJira := os.Getenv("JIRA_TOKEN"); envJira != "" {
		jiraToken = envJira
	}

	// telegramChatID: env > config.json > openclaw credentials (already read above)
	if cfg.TelegramChatID != "" && tgChatID == "" {
		tgChatID = cfg.TelegramChatID
	}
	upperName := strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
	if envChat := os.Getenv(upperName + "_TG_CHAT_ID"); envChat != "" {
		tgChatID = envChat
	}
	// backward compat: also check WEIRAN_TG_CHAT_ID
	if tgChatID == "" {
		if envChat := os.Getenv("WEIRAN_TG_CHAT_ID"); envChat != "" {
			tgChatID = envChat
		}
	}

	// agentName: config.json override (if openclaw.json didn't set it)
	if cfg.AgentName != "" && agentName == appName {
		agentName = cfg.AgentName
	}

	// projectRoots: expand ~ to $HOME
	for _, r := range cfg.ProjectRoots {
		if strings.HasPrefix(r, "~/") {
			r = filepath.Join(home, r[2:])
		}
		extraProjectRoots = append(extraProjectRoots, r)
	}
}

func init() {
	if buildVersion == "" {
		buildVersion = strings.TrimSpace(embeddedVersion)
	}
	initAppName()
	initWorkspace()
}

// initSessionDir creates /tmp/<agent>-MMDD-HHMM/ as the temp directory for this session
func initSessionDir() {
	name := agentName + "-" + time.Now().Format("0102-1504")
	sessionDir = filepath.Join(os.TempDir(), name)
	os.MkdirAll(sessionDir, 0700)
	promptOut = filepath.Join(sessionDir, "prompt.md")
}

// sessionTmp returns the file path under the session temp directory
func sessionTmp(name string) string {
	return filepath.Join(sessionDir, name)
}

// ── main ──

func helpText() string {
	upper := strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
	text := `{{NAME}} — Claude Code launcher with soul prompt

Usage:
  {{NAME}}                       interactive session (with soul prompt)
  {{NAME}} -p "task"             one-shot task, exits when done
  {{NAME}} -r                    resume session (opens TUI picker if no ID given)
  {{NAME}} -r <session-id>       resume specific session
  {{NAME}} --cron                memory consolidation mode (for scheduled tasks)
  {{NAME}} --heartbeat           heartbeat patrol mode
  {{NAME}} --evolve              self-evolution mode (daily cron, improves if inspired, skips otherwise)

Subcommands:
  {{NAME}} build                 safe build: backup current version -> go build -> verify -> auto-rollback on failure
  {{NAME}} versions              list historical versions
  {{NAME}} rollback [N]          rollback to Nth version (default 1 = previous)
  {{NAME}} update                self-update: git pull -> {{NAME}} build (safe build)
  {{NAME}} status                quick health check (without launching claude)
  {{NAME}} doctor                deep diagnostics (claude version, processes, DB, disk, model endpoints, metrics anomalies)
  {{NAME}} diff                  show soul/memory file changes since last commit
  {{NAME}} config                show current config (workspace, agent, paths)
  {{NAME}} log [N]               view daily notes (N=day offset, default today, 1=yesterday)
  {{NAME}} clean                 clean old session temp directories under /tmp
  {{NAME}} sessions [keyword]    search sessions (fuzzy match on title/content/project)
  {{NAME}} ss [keyword]          same as above (shorthand)
  {{NAME}} new                   reset all Telegram direct sessions (start new conversation)
  {{NAME}} notify <message>      send Telegram text message to user (supports Markdown)
  {{NAME}} notify-photo <URL> [caption]  send Telegram photo to user
  {{NAME}} db recall             view sessions pending scan and instructions
  {{NAME}} db pending            JSON output of sessions pending scan
  {{NAME}} db summarized         JSON output of sessions with summaries
  {{NAME}} db save '<json>'      save a single summary: {"path":"...","summary":"..."}
  {{NAME}} db save-batch '<json>' batch save summaries (also supports stdin: echo '[...]' | {{NAME}} db save-batch -)
  {{NAME}} db list               view last 20 records
  {{NAME}} db stats              show session count and summary count
  {{NAME}} db search <keyword>   search session summaries
  {{NAME}} db gc                 clean up DB records for deleted sessions
  {{NAME}} db patterns [-j]      list patterns (-j for JSON output)
  {{NAME}} db pattern-save '<json>'  save/update pattern: {"name":"...","description":"...","example":"...","source":"..."}
  {{NAME}} db pattern-save-batch batch save patterns (supports stdin)
  {{NAME}} db feedback '<json>'  record pattern feedback: {"pattern":"...","outcome":"success|failure|correction","note":"..."}
  {{NAME}} db cultivate [--dry-run]  run skill cultivation (auto-generates SKILL.md when threshold met)
  {{NAME}} db pattern-reject <name>  manually reject a pattern

Hooks:
  Post-hooks run automatically after claude finishes in cron/heartbeat mode:
  - Built-in: import summaries.json from session temp directory into DB
  - Custom: <appDir>/hooks/{cron,heartbeat}.d/*.sh

Environment variables:
  JIRA_TOKEN                Jira agent token (can also be set in config.json)
  {{UPPER}}_TG_CHAT_ID     Telegram chat ID (can also be set in config.json)
  {{UPPER}}_HOME            Base directory (default: ~/.openclaw)

Examples:
  {{NAME}}                              # start interactive session
  {{NAME}} -p "check my Jira backlog"   # one-shot task
  {{NAME}} status                       # quick health check
  {{NAME}} clean                        # clean temp files
  {{NAME}} notify "cron found anomaly"  # send notification to user
  {{NAME}} db recall                    # view sessions needing recall
  {{NAME}} db stats                     # check database status
  {{NAME}} --version                    # show version info
`
	text = strings.ReplaceAll(text, "{{NAME}}", appName)
	text = strings.ReplaceAll(text, "{{UPPER}}", upper)
	return text
}

func main() {
	// help / version
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--help", "-h", "help":
			fmt.Print(helpText())
			return
		case "--version", "-v", "version":
			fmt.Printf("%s %s (%s) built %s\n", appName, buildVersion, buildCommit, buildDate)
			return
		}
	}

	if jiraToken != "" {
		os.Setenv("JIRA_TOKEN", jiraToken)
	}

	mode, printPrompt, extra := parseArgs(os.Args[1:])
	currentMode = mode

	switch mode {
	case "db":
		handleDB(extra)
		return
	case "sessions":
		handleSessions(extra)
		return
	case "notify":
		msg := strings.Join(extra, " ")
		if msg == "" {
			fmt.Fprintf(os.Stderr, "usage: %s notify <message>\n", appName)
			os.Exit(1)
		}
		sendTelegram(msg)
		return
	case "notify-photo":
		if len(extra) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s notify-photo <image-URL> [caption]\n", appName)
			os.Exit(1)
		}
		caption := ""
		if len(extra) > 1 {
			caption = strings.Join(extra[1:], " ")
		}
		sendTelegramPhoto(extra[0], caption)
		return
	case "status":
		handleStatus()
		return
	case "log":
		handleLog(extra)
		return
	case "clean":
		handleClean()
		return
	case "build":
		handleBuild()
		return
	case "versions":
		handleVersions()
		return
	case "rollback":
		n := 1
		if len(extra) > 0 {
			if v, err := strconv.Atoi(extra[0]); err == nil && v >= 1 && v <= maxVersions {
				n = v
			}
		}
		handleRollback(n)
		return
	case "update":
		handleUpdate()
		return
	case "config":
		handleConfig()
		return
	case "doctor":
		handleDoctor()
		return
	case "diff":
		handleDiff()
		return
	case "new":
		handleNew()
		return
	}

	initSessionDir()
	result := buildPrompt()
	writePrompt(result)

	base := []string{"--append-system-prompt-file", promptOut, "--dangerously-skip-permissions"}

	switch mode {
	case "interactive":
		// -c (continue) / -r (resume) resumes an existing session, skip --name
		isResume := false
		hasResumeID := false
		for i, a := range extra {
			if a == "-c" || a == "--continue" || a == "-r" || a == "--resume" {
				isResume = true
				// check if next arg looks like a session ID (not another flag)
				if i+1 < len(extra) && !strings.HasPrefix(extra[i+1], "-") {
					hasResumeID = true
				}
				break
			}
		}
		// bare -r without session ID: open TUI to pick a session
		if isResume && !hasResumeID {
			sessions := scanAllSessions()
			sort.Slice(sessions, func(i, j int) bool {
				return sessions[i].ModTime.After(sessions[j].ModTime)
			})
			chosen := runSessionsTUI(sessions)
			if chosen == nil {
				fmt.Fprintln(os.Stderr, "["+appName+"] no session selected")
				return
			}
			// resume chosen session with soul prompt
			args := append(base, "--resume", chosen.ID)
			execClaude(args)
			return
		}
		args := base
		if !isResume {
			sessionName := agentName + "-" + time.Now().Format("0102-1504")
			args = append(args, "--name", sessionName)
		}
		args = append(args, extra...)
		execClaude(args)

	case "print":
		args := append(base, "-p", printPrompt)
		args = append(args, extra...)
		execClaude(args)

	case "cron":
		mustLock()
		defer releaseLock()

		// Pre-flight check
		if warnings := preflight(); len(warnings) > 0 {
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "["+appName+"] ⚠ preflight: %s\n", w)
			}
		}

		isWeekly := time.Now().Weekday() == time.Sunday
		if isWeekly {
			// Phase 1: haiku pre-scan, generate summaries
			fmt.Fprint(os.Stderr, "["+appName+"] Sunday deep review — phase 1: haiku pre-scan\n")
			scanTask := weeklyPreScan()
			scanArgs := append(base, "--model", "haiku", "--no-session-persistence", "-p", scanTask)
			if code := runClaude(scanArgs); code != 0 {
				fmt.Fprintf(os.Stderr, "[%s] haiku pre-scan failed (exit %d), skipping deep review, running normal cron\n", appName, code)
			} else {
				fmt.Fprint(os.Stderr, "["+appName+"] phase 1 complete, entering phase 2: opus deep review\n")
			}
		}

		task := cronTask()
		args := append(base, "-p", task)
		args = append(args, extra...)
		exitCode := runClaude(args)
		runHooks("cron")
		os.Exit(exitCode)

	case "heartbeat":
		mustLock()
		defer releaseLock()

		// Pre-flight check
		if warnings := preflight(); len(warnings) > 0 {
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "["+appName+"] ⚠ preflight: %s\n", w)
			}
		}

		hb := heartbeatTask()
		args := append(base, "-p", hb)
		args = append(args, extra...)
		exitCode := runClaude(args)
		runHooks("heartbeat")
		os.Exit(exitCode)

	case "evolve":
		mustLock()
		defer releaseLock()

		// Pre-flight check
		if warnings := preflight(); len(warnings) > 0 {
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "["+appName+"] ⚠ preflight: %s\n", w)
			}
		}

		ev := evolveTask()
		args := append(base, "-p", ev)
		args = append(args, extra...)
		exitCode := runClaude(args)
		runHooks("evolve")
		os.Exit(exitCode)
	}
}

// ── argument parsing ──

func parseArgs(args []string) (mode, printPrompt string, extra []string) {
	mode = "interactive"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "db":
			mode = "db"
			extra = args[i+1:]
			return
		case "sessions", "ss":
			mode = "sessions"
			extra = args[i+1:]
			return
		case "notify", "notify-photo":
			mode = args[i]
			extra = args[i+1:]
			return
		case "status":
			mode = "status"
			return
		case "log":
			mode = "log"
			extra = args[i+1:]
			return
		case "clean":
			mode = "clean"
			return
		case "build":
			mode = "build"
			extra = args[i+1:]
			return
		case "versions":
			mode = "versions"
			return
		case "rollback":
			mode = "rollback"
			extra = args[i+1:]
			return
		case "update":
			mode = "update"
			return
		case "config":
			mode = "config"
			return
		case "doctor":
			mode = "doctor"
			return
		case "diff":
			mode = "diff"
			return
		case "new":
			mode = "new"
			return
		case "--cron":
			mode = "cron"
		case "--heartbeat":
			mode = "heartbeat"
		case "--evolve":
			mode = "evolve"
		case "-p":
			mode = "print"
			if i+1 < len(args) {
				printPrompt = args[i+1]
				i++
			}
		default:
			extra = append(extra, args[i])
		}
	}
	return
}
