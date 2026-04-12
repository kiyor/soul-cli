package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	agentName      string // read from openclaw.json agents.list main agent name, defaults to appName
	agentAvatarURL    string // optional avatar image URL (from config.json "avatarUrl")
	agentWelcomeImage string // optional full-body welcome image URL (from config.json "welcomeImage")
	userAvatarURL     string // optional user avatar image URL (from config.json "userAvatarUrl")
	claudeBin = filepath.Join(home, ".local", "bin", "claude")
	lockfile  string // /tmp/<appName>.lock
	dbPath    string // <appDir>/sessions.db

	// Claude Code config directory (sessions, projects, settings).
	// Defaults to ~/.claude; override with CLAUDE_CONFIG_DIR env var for instance isolation.
	claudeConfigDir         string
	claudeConfigDirExplicit bool // true when CLAUDE_CONFIG_DIR was explicitly set by user

	// Per-session temp directory, initialized by initSessionDir()
	sessionDir string // /tmp/<agent>-0405-0924/
	promptOut  string // sessionDir/prompt.md

	jiraToken string // read from config.json or JIRA_TOKEN env var

	isServerMode    bool   // set to true when running as `weiran server`
	serverPort      int    // port the server is listening on (set in handleServer)
	serverAuthToken string // auth token for server API (set in handleServer)

	// Custom model override: --model provider/model (e.g. zai/glm-5.1, minimax/MiniMax-M2.7)
	// When set, injects provider's baseUrl + apiKey as ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN
	overrideModel string

	// Category override: --category cron|heartbeat|evolve
	// Used with -p to mark one-shot sessions as non-interactive in server_sessions DB.
	overrideCategory string

	// Default model for cron/heartbeat (from config.json "defaultModel")
	defaultModel string

	// Max heartbeat rounds before soul session auto-compact (from config.json "soulSessionMaxRounds")
	// 0 = disabled. Default 30 (~7.5h at 15-min intervals).
	soulSessionMaxRounds int

	launchDir  string // original working directory before chdir to workspace
	galContext string // GAL save JSON injected into prompt for resume

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
	appDir string // <appHome>/data/ (sessions.db, metrics, hooks, versions)
	srcDir string // source code directory (for build/update commands)

	// Service health check (from services.json or fallback defaults)
	monitoredServices []serviceEntry

	// Current run mode (cron/heartbeat/interactive etc.), used for metrics recording
	currentMode string


	// replaceSoul enables Id Mode (本我) — use --system-prompt-file (replace CC's
	// native system prompt) instead of --append-system-prompt-file.
	// DEFAULT: true (本我模式). Use --standard to switch to append mode.
	replaceSoul = true

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

	// Claude Code config dir isolation: CLAUDE_CONFIG_DIR env → default ~/.claude
	claudeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeConfigDir != "" {
		claudeConfigDirExplicit = true
	} else {
		claudeConfigDir = filepath.Join(home, ".claude")
	}
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

	// App data directory: <appHome>/data/ (XDG-style, independent of source tree)
	// Legacy: workspace/scripts/<appName>/ (auto-migrated)
	appDir = filepath.Join(appHome, "data")
	os.MkdirAll(appDir, 0755)

	// Migrate from legacy source-tree location if data exists there
	legacyDir := filepath.Join(workspace, "scripts", appName)
	migrateAppData(legacyDir, appDir)

	// Source directory: where go.mod lives (for build/update commands)
	// Try: workspace/scripts/<appName>/ → executable dir → not found
	srcDir = filepath.Join(workspace, "scripts", appName)
	if _, err := os.Stat(filepath.Join(srcDir, "go.mod")); err != nil {
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Dir(exe)
			if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
				srcDir = candidate
			}
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

	// projectRoots: workspace/projects + scripts always included, extras from config.json
	projectRoots = []string{
		filepath.Join(workspace, "projects"),
		filepath.Join(workspace, "scripts"),
	}
	projectRoots = append(projectRoots, extraProjectRoots...)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// migrateAppData moves data files from legacy source-tree location to the new appDir.
// Only migrates if the destination file does NOT already exist (no overwrite).
func migrateAppData(legacyDir, newDir string) {
	if legacyDir == newDir {
		return
	}
	if _, err := os.Stat(legacyDir); err != nil {
		return // legacy dir doesn't exist, nothing to migrate
	}

	dataFiles := []string{"sessions.db", "metrics.jsonl", "services.json", "config.json"}
	dataDirs := []string{".versions", "hooks"}

	migrated := 0
	for _, name := range dataFiles {
		src := filepath.Join(legacyDir, name)
		dst := filepath.Join(newDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if _, err := os.Stat(dst); err == nil {
			continue // already exists at destination
		}
		if err := os.Rename(src, dst); err != nil {
			// cross-device: copy + remove
			data, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			info, _ := os.Stat(src)
			if os.WriteFile(dst, data, info.Mode()) == nil {
				os.Remove(src)
				migrated++
			}
		} else {
			migrated++
		}
	}

	for _, name := range dataDirs {
		src := filepath.Join(legacyDir, name)
		dst := filepath.Join(newDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.Rename(src, dst); err == nil {
			migrated++
		}
		// skip cross-device dir copy — too complex, user can do manually
	}

	if migrated > 0 {
		fmt.Fprintf(os.Stderr, "[%s] migrated %d data items from %s → %s\n", appName, migrated, legacyDir, newDir)
	}
}

// loadConfig reads user configuration from config.json.
// Config file is located in the app data directory.
// All fields are optional; if unset, values are read from environment variables or openclaw.json.
func loadConfig() {
	configPaths := []string{
		filepath.Join(appHome, "data", "config.json"),               // new location
		filepath.Join(workspace, "scripts", appName, "config.json"), // legacy
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
		JiraToken            string   `json:"jiraToken"`
		TelegramChatID       string   `json:"telegramChatID"`
		ProjectRoots         []string `json:"projectRoots"`
		AgentName            string   `json:"agentName"`
		AvatarURL            string   `json:"avatarUrl"`            // optional avatar image URL for WebUI
		UserAvatarURL        string   `json:"userAvatarUrl"`        // optional user avatar image URL for WebUI
		WelcomeImage         string   `json:"welcomeImage"`         // optional full-body welcome page image URL
		DefaultModel         string   `json:"defaultModel"`         // default model for cron/heartbeat (e.g. "zai/glm-5.1")
		SoulSessionMaxRounds int      `json:"soulSessionMaxRounds"` // 0 = disabled, default 30
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

	// avatarUrl: config.json
	if cfg.AvatarURL != "" {
		agentAvatarURL = cfg.AvatarURL
	}
	if cfg.UserAvatarURL != "" {
		userAvatarURL = cfg.UserAvatarURL
	}

	// welcomeImage: config.json
	if cfg.WelcomeImage != "" {
		agentWelcomeImage = cfg.WelcomeImage
	}

	// projectRoots: expand ~ to $HOME
	for _, r := range cfg.ProjectRoots {
		if strings.HasPrefix(r, "~/") {
			r = filepath.Join(home, r[2:])
		}
		extraProjectRoots = append(extraProjectRoots, r)
	}

	// defaultModel: config.json > env var
	if cfg.DefaultModel != "" {
		defaultModel = cfg.DefaultModel
	}
	if envModel := os.Getenv("WEIRAN_DEFAULT_MODEL"); envModel != "" {
		defaultModel = envModel
	}

	// soulSessionMaxRounds: config.json (default 30)
	soulSessionMaxRounds = 30 // default
	if cfg.SoulSessionMaxRounds > 0 {
		soulSessionMaxRounds = cfg.SoulSessionMaxRounds
	}
}

func init() {
	if buildVersion == "" {
		buildVersion = strings.TrimSpace(embeddedVersion)
	}
	// Capture original working directory before any chdir
	launchDir, _ = os.Getwd()
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
  {{NAME}}                       interactive session (Id Mode 本我, default)
  {{NAME}} --standard            Standard mode — append soul to CC's native system prompt
  {{NAME}} -p "task"             one-shot task, exits when done
  {{NAME}} --standard -p "task"  one-shot task in Standard mode
  {{NAME}} --model glm            fuzzy match: "glm"→zai/glm-5.1, "highspeed"→minimax, "opus"→native
  {{NAME}} --model zai/glm-5.1    fully qualified (see '{{NAME}} models')
  {{NAME}} models                 list all available models (native + provider/model)
  {{NAME}} -r                    resume session (opens TUI picker if no ID given)
  {{NAME}} -r <session-id>       resume specific session
  {{NAME}} --cron                memory consolidation mode (for scheduled tasks)
  {{NAME}} --heartbeat           heartbeat patrol mode
  {{NAME}} --evolve              self-evolution mode (daily cron, improves if inspired, skips otherwise)
  {{NAME}} server                 HTTP API server (persistent Claude Code sessions)

Subcommands:
  {{NAME}} build                 safe build: backup current version -> go build -> verify -> auto-rollback on failure
  {{NAME}} versions              list historical versions
  {{NAME}} rollback [N]          rollback to Nth version (default 1 = previous)
  {{NAME}} update                self-update: git pull -> {{NAME}} build (safe build)
  {{NAME}} status                quick health check (without launching claude)
  {{NAME}} models                list available models grouped by provider (native + custom)
  {{NAME}} doctor                deep diagnostics (claude version, processes, DB, disk, model endpoints, metrics anomalies)
  {{NAME}} doctor cron           audit crontab: binary path, schedule sanity, log health, evolve-probe readiness
  {{NAME}} prompt                 print the full assembled system prompt to stdout
  {{NAME}} lint                   validate md file formats (topics, skills, CLAUDE.md)
  {{NAME}} diff                  show soul/memory file changes since last commit
  {{NAME}} config                show current config (workspace, agent, paths)
  {{NAME}} log [N]               view daily notes (N=day offset, default today, 1=yesterday)
  {{NAME}} server                 start HTTP API server for persistent Claude Code sessions
  {{NAME}} server --port 8080    custom port (default 9090)
  {{NAME}} server --host 0.0.0.0 expose to network (default 127.0.0.1)
  {{NAME}} server --token SECRET  set auth token (or use WEIRAN_SERVER_TOKEN env / config.json)
  {{NAME}} clean                 clean old session temp directories under /tmp
  {{NAME}} sessions [keyword]    search sessions (fuzzy match on title/content/project)
  {{NAME}} ss [keyword]          same as above (shorthand)
  {{NAME}} spawn <agent> "task"   dispatch task to another agent (async by default)
  {{NAME}} spawn <agent> "task" --wait  dispatch and wait for completion
  {{NAME}} spawn list            show running/recent spawn processes
  {{NAME}} evolve-probe -f <feedback> -s <scenario> [--mode with|without|both]
                                  run a thought-experiment probe against a feedback rule
  {{NAME}} evolve-probe -f <feedback> --list   list scenarios for a feedback
  {{NAME}} init                  first-run setup wizard (creates workspace + soul files)
  {{NAME}} init --yes            use defaults, no interactive prompts
  {{NAME}} init --force          overwrite existing files
  {{NAME}} init --archetype companion  use a personality archetype (companion/engineer/steward/mentor)
  {{NAME}} init --name X --role Y --personality Z --owner W --tz TZ  flag-driven (AI-friendly)
  {{NAME}} new                   reset all Telegram direct sessions (start new conversation)
  {{NAME}} notify [--dry-run] <message>      send Telegram text message to user (supports Markdown)
  {{NAME}} notify-photo [--dry-run] <URL> [caption]  send Telegram photo to user
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
		dryRun := false
		var msgParts []string
		for _, a := range extra {
			if a == "--dry-run" {
				dryRun = true
			} else {
				msgParts = append(msgParts, a)
			}
		}
		msg := strings.Join(msgParts, " ")
		if msg == "" {
			fmt.Fprintf(os.Stderr, "usage: %s notify [--dry-run] <message>\n", appName)
			os.Exit(1)
		}
		if dryRun {
			fmt.Printf("[dry-run] would send TG: %s\n", msg)
		} else {
			sendTelegram(msg)
		}
		return
	case "notify-photo":
		dryRun := false
		var photoParts []string
		for _, a := range extra {
			if a == "--dry-run" {
				dryRun = true
			} else {
				photoParts = append(photoParts, a)
			}
		}
		if len(photoParts) < 1 {
			fmt.Fprintf(os.Stderr, "usage: %s notify-photo [--dry-run] <image-URL> [caption]\n", appName)
			os.Exit(1)
		}
		caption := ""
		if len(photoParts) > 1 {
			caption = strings.Join(photoParts[1:], " ")
		}
		if dryRun {
			fmt.Printf("[dry-run] would send TG photo: %s (caption: %s)\n", photoParts[0], caption)
		} else {
			sendTelegramPhoto(photoParts[0], caption)
		}
		return
	case "status":
		handleStatus()
		return
	case "models":
		handleModels()
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
		if len(extra) > 0 && extra[0] == "cron" {
			handleDoctorCron()
		} else {
			handleDoctor()
		}
		return
	case "diff":
		handleDiff()
		return
	case "prompt":
		handlePrompt()
		return
	case "lint":
		handleLint()
		return
	case "new":
		handleNew()
		return
	case "spawn":
		handleSpawn(extra)
		return
	case "evolve-probe":
		handleProbe(extra)
		return
	case "init":
		handleInit(extra)
		return
	case "session":
		handleSessionIPC(extra)
		return
	case "server":
		handleServer(extra)
		return
	}

	initSessionDir()
	result := buildPrompt()
	writePrompt(result)

	// Apply defaultModel for automated modes only (cron/heartbeat/evolve).
	// Interactive mode intentionally uses the upstream default (Opus) so the
	// user gets the best model for conversational work. Automated jobs are
	// high-volume and benefit from a cheaper/faster provider. This is a
	// deliberate design choice — not a bug.
	if overrideModel == "" && defaultModel != "" {
		if mode == "cron" || mode == "heartbeat" || mode == "evolve" {
			overrideModel = defaultModel
			fmt.Fprintf(os.Stderr, "[%s] using defaultModel: %s\n", appName, defaultModel)
		}
	}

	promptFlag := "--append-system-prompt-file"
	if replaceSoul {
		promptFlag = "--system-prompt-file"
	} else {
		fmt.Fprintln(os.Stderr, "["+appName+"] Standard mode (--append-system-prompt-file)")
	}
	base := []string{promptFlag, promptOut, "--dangerously-skip-permissions"}

	// Inject --model flag for Claude Code when custom model is specified
	if overrideModel != "" {
		base = append(base, "--model", providerModelName(overrideModel))
	}

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
			// strip resume flags from extra; the rest (e.g. --chrome) is passed through
			var passthrough []string
			for _, a := range extra {
				if a == "-c" || a == "--continue" || a == "-r" || a == "--resume" {
					continue
				}
				passthrough = append(passthrough, a)
			}
			sessions := scanAllSessions()
			sort.Slice(sessions, func(i, j int) bool {
				return sessions[i].ModTime.After(sessions[j].ModTime)
			})
			chosen := runSessionsTUI(sessions)
			if chosen == nil {
				fmt.Fprintln(os.Stderr, "["+appName+"] no session selected")
				return
			}
			// resume chosen session with soul prompt + passthrough flags
			args := append(base, "--resume", chosen.ID)
			args = append(args, passthrough...)
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
		// If --category is set and server is running, delegate to server
		// so the session gets the correct category in server_sessions DB.
		if overrideCategory != "" && delegatePrintToServer(printPrompt, overrideCategory) {
			return
		}
		args := append(base, "-p", printPrompt)
		args = append(args, extra...)
		execClaude(args)

	case "cron":
		// If server is running, delegate to it so the session gets proper "cron" category.
		if delegatePrintToServer(cronTask(), CategoryCron) {
			// Post-delegate: FTS reindex + hooks still run locally.
			if added, skipped, err := indexDailyNotes(); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] fts reindex failed: %v\n", appName, err)
			} else if added > 0 {
				fmt.Fprintf(os.Stderr, "[%s] fts reindex: +%d daily notes (%d unchanged)\n", appName, added, skipped)
			}
			runHooks("cron")
			os.Exit(0)
		}

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
		beforeCron := time.Now()

		// P0: Log previous crash (metrics only, no prompt injection — cron is stateless)
		if crash := getLastCrashInfo("cron"); crash != nil {
			fmt.Fprintf(os.Stderr, "[%s] previous cron crashed (exit %d at %s): %s\n",
				appName, crash.ExitCode, crash.Timestamp, crash.Error)
			alertOnRepeatedCrash("cron")
		}

		args := append(base, "-p", task)
		args = append(args, extra...)
		exitCode := runClaude(args)

		// Track session ID for crash recovery metrics
		if newSID := detectNewSession(beforeCron); newSID != "" {
			lastClaudeSessionID = newSID
		}

		// Refresh FTS5 index for daily notes after memory consolidation.
		// Best-effort: failure here never blocks cron completion.
		if added, skipped, err := indexDailyNotes(); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] fts reindex failed: %v\n", appName, err)
		} else if added > 0 {
			fmt.Fprintf(os.Stderr, "[%s] fts reindex: +%d daily notes (%d unchanged)\n", appName, added, skipped)
		}

		runHooks("cron")
		os.Exit(exitCode)

	case "heartbeat":
		// If server is running, delegate to /api/wake instead of spawning a subprocess.
		// This ensures heartbeat sessions get proper category in server_sessions DB.
		if delegateToServer("heartbeat") {
			os.Exit(0)
		}

		mustLock()
		defer releaseLock()

		// Pre-flight check
		if warnings := preflight(); len(warnings) > 0 {
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "["+appName+"] ⚠ preflight: %s\n", w)
			}
		}

		// Expire stale soul sessions (>24h inactive)
		endStaleSoulSessions()

		hb := heartbeatTask()
		beforeRun := time.Now()

		// P0: Log previous crash (metrics only, no prompt injection — heartbeat is stateless)
		if crash := getLastCrashInfo("heartbeat"); crash != nil {
			fmt.Fprintf(os.Stderr, "[%s] previous heartbeat crashed (exit %d at %s): %s\n",
				appName, crash.ExitCode, crash.Timestamp, crash.Error)
			alertOnRepeatedCrash("heartbeat")
		}

		// Check for active soul session to resume
		if claudeSID := getActiveSoulSession(agentName); claudeSID != "" {
			fmt.Fprintf(os.Stderr, "["+appName+"] resuming soul session (claude %s)\n", shortID(claudeSID))
			lastClaudeSessionID = claudeSID
			args := append(base, "--resume", claudeSID, "-p", hb)
			args = append(args, extra...)
			exitCode := runClaude(args)
			touchSoulSession(agentName)
			updateSoulSessionBudget(agentName)
			runHooks("heartbeat")
			os.Exit(exitCode)
		}

		// No active session — create new daily soul session
		soulID := createSoulSession(agentName, "daily", 2.0)
		args := append(base, "-p", hb)
		args = append(args, extra...)
		exitCode := runClaude(args)

		// Detect and link the Claude session ID created by this run
		if soulID > 0 {
			if newSID := detectNewSession(beforeRun); newSID != "" {
				lastClaudeSessionID = newSID
				linkSoulSession(soulID, newSID)
			}
		}

		runHooks("heartbeat")
		os.Exit(exitCode)

	case "evolve":
		// If server is running, delegate to it so the session gets proper "evolve" category.
		if delegatePrintToServer(evolveTask(), CategoryEvolve) {
			runHooks("evolve")
			os.Exit(0)
		}

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

// delegateToServer tries to delegate a heartbeat/cron/evolve run to the running server.
// Returns true if the server handled it (caller should exit), false if server is not running.
func delegateToServer(mode string) bool {
	cfg := loadServerConfig()
	addr := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)

	var endpoint, reason string
	switch mode {
	case "heartbeat":
		endpoint = "/api/wake"
		reason = "cron-heartbeat"
	default:
		return false
	}

	payload := map[string]string{"text": reason}
	if defaultModel != "" {
		payload["model"] = defaultModel
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(addr+endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server not reachable, falling back to subprocess mode\n", appName)
		return false
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		fmt.Fprintf(os.Stderr, "[%s] delegated %s to server: %s\n", appName, mode, string(respBody))
		return true
	}
	if resp.StatusCode == http.StatusConflict {
		// Already running — that's fine, skip this cycle
		fmt.Fprintf(os.Stderr, "[%s] server: %s session already running, skipping\n", appName, mode)
		return true
	}

	fmt.Fprintf(os.Stderr, "[%s] server returned %d for %s, falling back to subprocess\n", appName, resp.StatusCode, mode)
	return false
}

// delegatePrintToServer sends a -p task to the running server with a specific category.
// Returns true if the server handled it, false if server is not running.
func delegatePrintToServer(prompt, category string) bool {
	cfg := loadServerConfig()
	addr := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)

	name := fmt.Sprintf("%s-%s-%s", agentName, category, time.Now().Format("0102-1504"))
	payload := map[string]interface{}{
		"name":            name,
		"category":        category,
		"initial_message": prompt,
		"soul_files":      replaceSoul,
	}
	if overrideModel != "" {
		payload["model"] = overrideModel
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("POST", addr+"/api/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] server not reachable, falling back to direct exec\n", appName)
		return false
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		fmt.Fprintf(os.Stderr, "[%s] delegated -p to server (category=%s): %s\n", appName, category, string(respBody))
		return true
	}

	fmt.Fprintf(os.Stderr, "[%s] server returned %d for -p delegation, falling back to direct exec\n", appName, resp.StatusCode)
	return false
}

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
		case "models":
			mode = "models"
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
			extra = args[i+1:]
			return
		case "diff":
			mode = "diff"
			return
		case "prompt":
			mode = "prompt"
			return
		case "lint":
			mode = "lint"
			return
		case "new":
			mode = "new"
			return
		case "spawn":
			mode = "spawn"
			extra = args[i+1:]
			return
		case "evolve-probe":
			mode = "evolve-probe"
			extra = args[i+1:]
			return
		case "init":
			mode = "init"
			extra = args[i+1:]
			return
		case "session":
			mode = "session"
			extra = args[i+1:]
			return
		case "server":
			mode = "server"
			extra = args[i+1:]
			return
		case "--cron":
			mode = "cron"
		case "--heartbeat":
			mode = "heartbeat"
		case "--evolve":
			mode = "evolve"
		case "--id", "--soul":
			// already default, no-op for backward compat
			replaceSoul = true
		case "--standard":
			// Standard mode: append to CC's native system prompt instead of replacing
			replaceSoul = false
		case "--model":
			// Custom model: supports fuzzy matching (e.g. "glm", "highspeed", "minimax", "opus")
			if i+1 < len(args) {
				resolved, err := resolveFuzzyModel(args[i+1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "[%s] %v\n", appName, err)
					os.Exit(1)
				}
				if resolved != "" {
					overrideModel = resolved
				}
				i++
			}
		case "--category":
			if i+1 < len(args) {
				overrideCategory = args[i+1]
				i++
			}
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
