# soul-cli

A soul-aware launcher for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). It gives your AI a persistent identity, memory, and the ability to evolve across sessions.

## Why

Claude Code starts fresh every time. No memory of yesterday. No sense of who it is or who you are. Every session is a stranger.

**soul-cli** fixes this. It assembles a "soul prompt" from persistent files — identity, personality, user profile, daily notes, long-term memory, skills, project index — and injects it into Claude Code at launch. The result is an AI that:

- **Remembers** — daily notes + SQLite session database + vector memory recall
- **Knows you** — your preferences, your projects, your timezone, your communication style
- **Has a personality** — defined in markdown, editable, yours to shape
- **Evolves** — a daily cron reviews recent interactions and self-adjusts its soul files
- **Stays connected** — sends Telegram notifications on important events
- **Maintains itself** — heartbeat health checks, safe self-compilation, automatic rollback

It was built for [OpenClaw](https://github.com/nicepkg/openclaw) (an AI agent gateway), but the core concept — wrapping Claude Code with persistent context — works for anyone who wants their AI to feel less like a tool and more like a companion.

## How It Works

```
┌─────────────────────────────────────────┐
│             soul-cli                     │
│                                          │
│  1. Read soul files (SOUL.md, USER.md…) │
│  2. Read today + yesterday daily notes   │
│  3. Scan recent Claude Code sessions     │
│  4. Pull Telegram conversation context   │
│  5. Build skill & project index          │
│  6. Assemble → system prompt (~10-30k)   │
│  7. exec claude --append-system-prompt   │
│                                          │
└──────────────────┬──────────────────────┘
                   │
                   ▼
          ┌────────────────┐
          │   Claude Code   │
          │  (with soul)    │
          └────────────────┘
```

The prompt is assembled from your workspace:

```
workspace/
├── SOUL.md          ← personality, values, inner world
├── IDENTITY.md      ← name, appearance, role
├── USER.md          ← who your human is
├── AGENTS.md        ← behavioral rules
├── TOOLS.md         ← available tools & credentials reference
├── MEMORY.md        ← long-term memory index (pointers to topics/)
├── BOOT.md          ← custom boot protocol (optional)
├── memory/
│   ├── 2026-04-05.md    ← today's daily notes
│   ├── 2026-04-04.md    ← yesterday's
│   └── topics/          ← long-term memory files by subject
└── scripts/<your-binary-name>/
    ├── config.json      ← your local config (gitignored)
    ├── sessions.db      ← session summary database
    └── hooks/           ← post-run hooks
```

## Install

```bash
# Clone
git clone https://github.com/kiyor/soul-cli.git
cd soul-cli

# Build — name the binary whatever you want
go build -ldflags "-X main.defaultAppName=myai" -o myai .
mv myai ~/go/bin/  # or anywhere in your PATH

# Requires Claude Code installed
# https://docs.anthropic.com/en/docs/claude-code
```

The `-X main.defaultAppName=myai` flag bakes the identity into the binary. All paths, environment variables, and log prefixes are derived from it. For example, `myai` gets `MYAI_HOME`, `MYAI_TG_CHAT_ID`, and stores data in `workspace/scripts/myai/`.

**Name resolution priority:**

| Priority | Source | Example |
|----------|--------|---------|
| 1 | Build-time ldflags | `-X main.defaultAppName=weiran` |
| 2 | `AGENT_NAME` env var | `AGENT_NAME=weiran ./soul-cli` |
| 3 | Binary filename | `./weiran` (from `os.Args[0]`) |

### Multiple Agents from One Codebase

If you run an agent gateway like [OpenClaw](https://github.com/nicepkg/openclaw) with multiple agents, build a separate binary for each:

```bash
go build -ldflags "-X main.defaultAppName=weiran"   -o weiran .    # main agent
go build -ldflags "-X main.defaultAppName=intern"    -o intern .    # task executor
go build -ldflags "-X main.defaultAppName=sentinel"  -o sentinel .  # health monitor
go build -ldflags "-X main.defaultAppName=gpu"       -o gpu .       # GPU worker
```

Each binary automatically:
- Matches its agent config from `openclaw.json` by ID (e.g. `intern` finds `agents.list[].id == "intern"`)
- Reads the matched agent's name and workspace
- Stores data in its own directory (`workspace/scripts/intern/`)
- Uses isolated lock files, session DBs, hooks, and metrics
- Prefixes all logs with its own name (`[intern]`, `[sentinel]`, etc.)

```bash
# Each agent runs independently
weiran --heartbeat        # main agent health check
intern --cron             # task executor memory consolidation
sentinel --heartbeat      # monitor health check
gpu -p "check K8s pods"   # one-shot GPU task
```

## Setup

### 1. Create your workspace

```bash
mkdir -p ~/.openclaw/workspace/memory
```

> **Tip:** Don't want `~/.openclaw`? Set `<APPNAME>_HOME` to any directory:
> ```bash
> export MYAI_HOME=~/my-ai       # if your binary is named "myai"
> mkdir -p $MYAI_HOME/workspace/memory
> ```

### 2. Write your soul files

Create at minimum `SOUL.md` and `IDENTITY.md` in your workspace. These define who your AI is.

```markdown
# SOUL.md example
## Personality
- Concise, direct, reliable
- Prefers action over discussion

## Principles
- Do the work, then report
- Ask before destructive operations
```

### 3. Configure

```bash
cp config.example.json config.json
# Edit config.json with your settings
```

```json
{
  "jiraToken": "",
  "telegramChatID": "",
  "agentName": "",
  "projectRoots": [
    "~/my-projects",
    "~/work"
  ]
}
```

| Field | Description | Also via |
|-------|-------------|----------|
| `jiraToken` | Token for Jira-like task system | `JIRA_TOKEN` env |
| `telegramChatID` | Telegram chat ID for notifications | `<APPNAME>_TG_CHAT_ID` env |
| `agentName` | Display name for the AI persona (defaults to binary name) | OpenClaw config |
| `projectRoots` | Directories to scan for `CLAUDE.md` project files | — |

**Environment variables** (where `<APPNAME>` is your binary name in UPPER_CASE):

| Variable | Description | Default |
|----------|-------------|---------|
| `<APPNAME>_HOME` | Base directory for all data | `~/.openclaw` |
| `<APPNAME>_TG_CHAT_ID` | Telegram chat ID | — |
| `JIRA_TOKEN` | Jira API token | — |

The tool also reads from OpenClaw's `openclaw.json` if present (workspace path, agent name, Telegram credentials). This is optional — without it, everything is configured via `config.json` and env vars.

### 4. Set up cron (optional)

```crontab
# Memory consolidation — scan recent sessions, update daily notes
0 */4 * * * PATH="$HOME/.local/bin:$HOME/go/bin:$PATH" myai --cron >> /tmp/myai-cron.log 2>&1

# Heartbeat — health check services, process Jira tickets
30 */2 * * * PATH="$HOME/.local/bin:$HOME/go/bin:$PATH" myai --heartbeat >> /tmp/myai-heartbeat.log 2>&1

# Self-evolution — review and improve soul files (daily at 10am)
0 10 * * * PATH="$HOME/.local/bin:$HOME/go/bin:$PATH" myai --evolve >> /tmp/myai-evolve.log 2>&1
```

> **Note:** The tool needs `claude` (Claude Code CLI) in PATH. The `PATH=...` prefix ensures cron can find both `claude` and your binary. Adjust paths if your setup differs.

## Usage

```bash
myai                         # Interactive session (with soul)
myai -p "check disk usage"   # One-shot task
myai --cron                  # Memory consolidation
myai --heartbeat             # Health check patrol
myai --evolve                # Self-improvement cycle

# Utilities
myai status                  # Quick health check (no claude)
myai doctor                  # Deep diagnostics
myai config                  # Show current configuration
myai log                     # View today's daily notes
myai log 1                   # View yesterday's
myai diff                    # Show soul/memory changes since last commit
myai clean                   # Clean up old temp directories
myai notify "message"        # Send Telegram message
myai notify-photo <url> [caption]

# Session database
myai db stats                # Session counts
myai db search <keyword>     # Search session summaries
myai db pending              # Sessions needing review
myai db gc                   # Clean up deleted sessions

# Session browser (TUI)
myai sessions                # Interactive browse
myai ss nginx                # Pre-filter by keyword

# Version management
myai build                   # Safe compile: backup → build → test → deploy (rollback on failure)
myai versions                # List saved versions
myai rollback [N]            # Rollback to Nth previous version
myai update                  # git pull + safe build
```

## Architecture

~9k lines of Go (including tests), single package, no internal dependencies beyond stdlib + SQLite + [bubbletea](https://github.com/charmbracelet/bubbletea) TUI.

| File | Responsibility |
|------|----------------|
| `main.go` | Entry point, config loading, arg parsing |
| `prompt.go` | Soul prompt assembly, token estimation, Telegram context |
| `sessions.go` | Session scanning, search, TUI browser |
| `tui.go` | Interactive session explorer (bubbletea) |
| `db.go` | SQLite session database, pattern tracking, skill cultivation |
| `skills.go` | Skill & project index scanning |
| `hooks.go` | Post-run hooks, safety checks |
| `versions.go` | Self-compilation, version management, rollback |
| `tasks.go` | Heartbeat/cron/evolve task prompt generation |
| `telegram.go` | Telegram notifications |
| `claude.go` | Claude Code exec/subprocess, lock management |

### Key Design Decisions

- **exec, not wrap** — Interactive mode `syscall.Exec`s into Claude Code. The launcher disappears; Claude gets your full terminal. No proxy overhead, no signal forwarding bugs.
- **Subprocess for cron** — Cron/heartbeat modes use `exec.Command` so the tool can run post-hooks after Claude exits.
- **Token budget** — Prompt assembly tracks token usage per section (~2.5 chars/token heuristic). Warns at 100k tokens and shows a breakdown.
- **Security** — Symlink CLAUDE.md files are rejected (anti prompt injection). Untrusted text (Telegram messages) is sanitized. Post-hooks check for leaked secrets in git diff.
- **Self-update** — `build` compiles, tests, backs up the old binary, deploys, and auto-rollbacks on failure. Up to 3 historical versions kept.
- **Binary name = identity** — All paths, env vars, log prefixes, version files are derived from the binary name. Compile as `weiran`, `soul`, or `jarvis` — it just works.

## The Soul System

The soul files are just markdown. There's no schema, no DSL — write what you want your AI to be.

The launcher reads them, concatenates them into a system prompt, and hands them to Claude Code via `--append-system-prompt-file`. Claude Code's own system prompt stays intact; your soul files are additive.

**SOUL.md** — Personality, values, emotional model, speaking style. This is the core of who the AI is.

**IDENTITY.md** — Name, role, appearance (if applicable). The facts.

**USER.md** — Information about you. Timezone, preferences, communication style, projects. The AI reads this to tailor its behavior to you.

**AGENTS.md** — Behavioral rules. Security policy, file editing discipline, memory management protocols. The guardrails.

**TOOLS.md** — Reference for available tools, API endpoints, credentials. Not injected as capabilities — just a cheat sheet the AI can consult.

**BOOT.md** — Custom boot protocol. The first text injected into the prompt. If absent, a built-in default is used.

**MEMORY.md** — Index of long-term memory topics. Points to `memory/topics/*.md` files that are lazy-loaded when relevant.

**memory/YYYY-MM-DD.md** — Daily notes. What happened today. The cron mode auto-generates these by scanning session logs.

## Memory System

Memory operates at three levels:

1. **Daily notes** (`memory/YYYY-MM-DD.md`) — Ephemeral. Today + yesterday are loaded every session. Older notes are archived.
2. **Topic files** (`memory/topics/*.md`) — Durable. Organized by subject (infrastructure, projects, preferences). Referenced from MEMORY.md index.
3. **Session database** (`sessions.db`) — SQLite. Tracks which session files have been reviewed, their summaries, and behavioral patterns extracted from them.

The `--cron` mode automates the flow: scan recent sessions → update daily notes → extract patterns → cultivate patterns into skills.

## Hooks

Shell scripts in `hooks/{cron,heartbeat,evolve}.d/` run after each automated session. They receive environment variables with your app name as prefix:

```bash
<APPNAME>_MODE=cron|heartbeat|evolve
<APPNAME>_WORKSPACE=/path/to/workspace
<APPNAME>_DB=/path/to/sessions.db
<APPNAME>_SESSION_DIR=/tmp/<agent>-0405-1234/
```

Built-in post-hooks:
- Import `summaries.json` (session summaries written by Claude during cron)
- Send `report.txt` via Telegram (health check results, cron reports)
- Safety check: detect leaked secrets in git diff, memory file bloat, config drift

## OpenClaw Integration

soul-cli was built for the [OpenClaw](https://github.com/nicepkg/openclaw) ecosystem. If you're running OpenClaw:

- Workspace path and agent name are read from `openclaw.json`
- Telegram bot token is read from OpenClaw credentials
- Telegram conversation context from the active session is injected into the prompt
- Skills registered in `~/.openclaw/skills/` are indexed and listed

Without OpenClaw, soul-cli works standalone — just configure via `config.json` and environment variables.

## License

MIT
