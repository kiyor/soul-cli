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

Multi-file Go program (package main, ~6000 lines across 14 files) with no internal packages.

| File | Lines | Responsibility |
|------|-------|----------------|
| `main.go` | ~650 | Globals, init, main(), parseArgs, helpText, `-r` TUI picker |
| `db.go` | ~800 | SQLite DB operations, handleDB subcommands, pattern cultivation |
| `status.go` | ~800 | Health check, diagnostics, config display, diff viewer, metrics |
| `prompt.go` | ~680 | buildPrompt, Telegram context, token estimation, sanitize |
| `sessions.go` | ~585 | Session search/scan/filter, TUI entry, recentSessions |
| `tui.go` | ~475 | Bubbletea TUI for session explorer |
| `tasks.go` | ~405 | Heartbeat/cron/evolve task templates (text/template) |
| `hooks.go` | ~375 | Post-hooks, safetyCheck, deliverReport, failure streak detection |
| `skills.go` | ~320 | Skill/project index scanning, dir exclusion |
| `claude.go` | ~315 | exec/run claude, lock management, metrics tracking |
| `versions.go` | ~275 | Version management, build/rollback |
| `telegram.go` | ~160 | Telegram send message/photo (with token caching) |
| `new.go` | ~110 | Reset Telegram sessions |
| `safe.go` | ~105 | Symlink protection, NFS-safe lock |

Key subsystems:

**Modes** (selected via CLI args in `parseArgs`):
- `interactive` — default, `syscall.Exec` replaces process with `claude`
- `-r` / `-c` — resume session; bare `-r` opens TUI session picker
- `-p "task"` — one-shot task, also uses `syscall.Exec`
- `--cron` — memory consolidation (subprocess `runClaude`, then post-hooks). On Sundays, runs a two-phase deep review: haiku pre-scan then opus consolidation
- `--heartbeat` — health check patrol (subprocess `runClaude`, then post-hooks)
- `--evolve` — self-evolution: audits code, fixes bugs, updates soul/memory, builds & commits
- `notify` / `notify-photo` — send Telegram messages directly
- `db` subcommands — manage the session summary SQLite database

**Prompt Assembly** (`buildPrompt`): Concatenates startup protocol + soul files (SOUL.md, IDENTITY.md, USER.md, AGENTS.md, TOOLS.md) + MEMORY.md + today/yesterday daily notes + skill index + project index. Token budget is 100k tokens with a `estimateTokens` heuristic.

**Session DB** (SQLite via `modernc.org/sqlite`): Tracks which JSONL session files have been summarized. Uses SHA-256 hash (head+tail for large files) to detect changes. DB path: `<appHome>/data/sessions.db`.

**FTS5 Full-Text Search**: Three external-content FTS5 virtual tables over the session DB:
1. `daily_notes_fts` — indexes `workspace/memory/*.md` daily diary files
2. `session_summaries_fts` — indexes session summaries (heart-beat patrol notes)
3. `session_content_fts` — indexes extracted user/assistant text from session JSONL files (both OpenClaw and Claude Code sessions). Handles both JSONL formats: OpenClaw (`type:"message"`) and Claude Code (`type:"user"/"assistant"`).

Key subcommands:
- `weiran db fts-index` — incrementally index daily notes + session content
- `weiran db fts-index-sessions` — index session JSONL content only
- `weiran db fts-rebuild` — rebuild all FTS5 indexes from source tables
- `weiran db search-fts <query> [--scope=daily|session|content|both] [--limit=N] [--json]` — unified BM25-ranked search

**Post-Hooks** (`runHooks`): After cron/heartbeat/evolve runs, executes: (1) import summaries.json into DB, (2) send report.txt via Telegram, (3) safety check (soul files, memory bloat, sensitive info in git diff, config drift), (4) user scripts in `hooks/{cron,heartbeat,evolve}.d/*.sh`.

**Skill/Project Index**: Scans `~/.openclaw/skills/` and `~/.openclaw/workspace/skills/` for `SKILL.md` files with YAML frontmatter. Scans multiple project roots for `CLAUDE.md` files. Both produce markdown tables injected into the prompt.

## Key Paths & Constants

- `workspace` = `~/.openclaw/workspace` (soul files, memory, projects)
- `claudeBin` = `~/.local/bin/claude`
- `lockfile` = `/tmp/weiran.lock` (prevents concurrent cron/heartbeat)
- `promptOut` = `/tmp/weiran-prompt-active.md` (assembled prompt written here)
- `dbPath` = `<appHome>/data/sessions.db` (SQLite)
- `tgChatID` = Telegram chat ID (read from config.json / openclaw.json / env var)

## Coding Rules

- **Framework vs user data separation**: Framework code must only match text produced by the framework itself (e.g. templates in `tasks.go`). User skill names, prompt content, and behavioral patterns are private data — never hardcode them into framework logic. soul-cli is open-source; code must not leak any specific user's usage patterns.

## Hooks

Shell scripts in `hooks/{cron,heartbeat}.d/` run after each cron/heartbeat session. They receive env vars: `WEIRAN_MODE`, `WEIRAN_WORKSPACE`, `WEIRAN_DB`. Currently `90-safety-extended.sh` does supplementary checks (memory pollution, git health, temp file cleanup, openclaw.json validity).
