# soul-cli

A soul-aware launcher for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). It gives your AI a persistent identity, memory, and the ability to evolve across sessions.

Claude Code starts fresh every time — no memory, no personality, no idea who you are. **soul-cli** fixes this by assembling markdown files (identity, personality, memory, skills) into a system prompt injected at launch.

## Quick Start

Tell Claude Code to set it up for you:

> Clone https://github.com/kiyor/soul-cli, build it as `myai`, set up a workspace with soul files. My name is Alex, I'm a backend engineer, timezone US Pacific.

Or manually:

```bash
git clone https://github.com/kiyor/soul-cli.git && cd soul-cli
go build -ldflags "-X main.defaultAppName=myai" -o myai .
mv myai ~/go/bin/

mkdir -p ~/.openclaw/workspace/memory
# Write SOUL.md + IDENTITY.md in ~/.openclaw/workspace/

myai  # Claude Code, but it remembers
```

> **Requires:** Go 1.21+ and [Claude Code](https://docs.anthropic.com/en/docs/claude-code)

## How It Works

```
Soul Files + Memory + Skills  →  soul-cli  →  Claude Code (with soul)
     (markdown)                  (assembles)    (--append-system-prompt)
```

- **`SOUL.md`** — personality, values, speaking style
- **`USER.md`** — your timezone, preferences, expertise
- **`MEMORY.md`** + daily notes — what happened yesterday, long-term knowledge
- **`AGENTS.md`** — behavioral rules and guardrails

The binary name determines identity: build as `myai`, `jarvis`, or `atlas` — all paths, env vars, and logs derive from it.

## Features

| Feature | How |
|---------|-----|
| **Memory** | Daily notes auto-generated from sessions, long-term topics, SQLite session DB |
| **Evolution** | `--evolve` cron reviews interactions and self-adjusts soul files |
| **Server mode** | Built-in HTTP server + Web UI for persistent sessions |
| **Automation** | `--cron` (memory), `--heartbeat` (health checks), `--evolve` (self-improvement) |
| **Multi-agent** | One codebase, multiple binaries with isolated data |
| **Safety** | Symlink rejection, secret leak detection, CORE.md protection, auto-rollback |
| **Telegram** | Notifications, reports, conversation context injection |

## Usage

```bash
myai                         # interactive session
myai -p "check disk usage"   # one-shot task
myai -r                      # resume previous session
myai server --token secret   # HTTP server + Web UI
myai --cron                  # memory consolidation
myai --heartbeat             # health check patrol
myai --evolve                # self-improvement
myai status                  # quick diagnostics
```

## Documentation

**[Full documentation →](https://kiyor.github.io/soul-cli/)**

- [Getting Started](https://kiyor.github.io/soul-cli/getting-started/) — install, configure, launch
- [Core Concepts](https://kiyor.github.io/soul-cli/concepts/) — soul files, memory, evolution
- [Soul Files Guide](https://kiyor.github.io/soul-cli/guides/soul-files/) — writing each markdown file
- [Server Mode](https://kiyor.github.io/soul-cli/guides/server/) — HTTP API, Web UI, deployment
- [Automation](https://kiyor.github.io/soul-cli/guides/automation/) — cron, heartbeat, evolve
- [CLI Reference](https://kiyor.github.io/soul-cli/reference/cli/) — every command and flag
- [API Reference](https://kiyor.github.io/soul-cli/reference/api/) — server endpoints
- [FAQ](https://kiyor.github.io/soul-cli/faq/)

## License

MIT
