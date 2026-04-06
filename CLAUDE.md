# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

`soul-cli` is a Go CLI that launches Claude Code with a "soul prompt". It assembles identity/memory/skill files into a system prompt, then `exec`s or subprocess-runs `claude` with that prompt. It also handles cron-based memory consolidation, heartbeat health checks, Telegram notifications, and a session summary database. The binary name is configurable — compile as `weiran`, `soul`, or any name you want; all paths and env vars are derived from it.

## Build & Run

```bash
go build -o soul .          # build (name it whatever you want)
go test ./...               # run all tests
go test -run TestBuildSkillIndex  # single test
```

No external services needed for tests (uses temp dirs and in-memory SQLite). The `TestBuildSkillIndex` and `TestBuildPrompt` tests read real files from `~/.openclaw/` so they only pass on machines with a configured workspace.

## Architecture

Multi-file Go program (package main, ~3200 lines across 11 files) with no internal packages.

| File | Lines | Responsibility |
|------|-------|----------------|
| `main.go` | ~325 | Globals, init, main(), parseArgs, helpText |
| `prompt.go` | ~390 | buildPrompt, Telegram context, token estimation, sanitize |
| `sessions.go` | ~585 | Session search/scan/filter, TUI entry, recentSessions |
| `tui.go` | ~475 | Bubbletea TUI for session explorer |
| `db.go` | ~300 | SQLite DB operations, handleDB subcommands, importSummaries |
| `skills.go` | ~280 | Skill/project index scanning |
| `hooks.go` | ~230 | Post-hooks, safetyCheck, deliverReport |
| `versions.go` | ~220 | Version management, build/rollback |
| `tasks.go` | ~215 | Heartbeat/cron/weekly task text generation |
| `telegram.go` | ~100 | Telegram send message/photo |
| `claude.go` | ~100 | exec/run claude, lock management |

Key subsystems:

**Modes** (selected via CLI args in `parseArgs`):
- `interactive` — default, `syscall.Exec` replaces process with `claude`
- `-p "task"` — one-shot task, also uses `syscall.Exec`
- `--cron` — memory consolidation (subprocess `runClaude`, then post-hooks). On Sundays, runs a two-phase deep review: haiku pre-scan then opus consolidation
- `--heartbeat` — health check patrol (subprocess `runClaude`, then post-hooks)
- `notify` / `notify-photo` — send Telegram messages directly
- `db` subcommands — manage the session summary SQLite database

**Prompt Assembly** (`buildPrompt`): Concatenates startup protocol + soul files (SOUL.md, IDENTITY.md, USER.md, AGENTS.md, TOOLS.md) + MEMORY.md + today/yesterday daily notes + skill index + project index. Token budget is 100k tokens with a `estimateTokens` heuristic.

**Session DB** (SQLite via `modernc.org/sqlite`): Tracks which JSONL session files have been summarized. Uses SHA-256 hash (head+tail for large files) to detect changes. DB path: `sessions.db` in this directory.

**Post-Hooks** (`runHooks`): After cron/heartbeat runs, executes: (1) import `/tmp/weiran-summaries.json` into DB, (2) send `/tmp/weiran-report.txt` via Telegram, (3) safety check (soul files, memory bloat, sensitive info in git diff, config drift), (4) user scripts in `hooks/{cron,heartbeat}.d/*.sh`.

**Skill/Project Index**: Scans `~/.openclaw/skills/` and `~/.openclaw/workspace/skills/` for `SKILL.md` files with YAML frontmatter. Scans multiple project roots for `CLAUDE.md` files. Both produce markdown tables injected into the prompt.

## Key Paths & Constants

- `workspace` = `~/.openclaw/workspace` (soul files, memory, projects)
- `claudeBin` = `~/.local/bin/claude`
- `lockfile` = `/tmp/weiran.lock` (prevents concurrent cron/heartbeat)
- `promptOut` = `/tmp/weiran-prompt-active.md` (assembled prompt written here)
- `dbPath` = `./sessions.db` (SQLite, in this directory)
- `tgChatID` = Telegram chat ID (read from config.json / openclaw.json / env var)

## Hooks

Shell scripts in `hooks/{cron,heartbeat}.d/` run after each cron/heartbeat session. They receive env vars: `WEIRAN_MODE`, `WEIRAN_WORKSPACE`, `WEIRAN_DB`. Currently `90-safety-extended.sh` does supplementary checks (memory pollution, git health, temp file cleanup, openclaw.json validity).
