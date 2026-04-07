# Getting Started

## The Fast Way (Let Claude Code Do It)

You're already using Claude Code, right? Just tell it:

> Clone https://github.com/kiyor/soul-cli, build it as `myai`, set up a workspace, and write initial soul files for me. My name is Alex, I'm a backend engineer, timezone US Pacific.

Claude Code will handle the rest — clone, build, create workspace, write SOUL.md / IDENTITY.md / USER.md. Done.

If you want more control, keep reading.

## Manual Setup

### Prerequisites

- **Go 1.21+** — [Install Go](https://go.dev/dl/)
- **Claude Code** — [Install Claude Code](https://docs.anthropic.com/en/docs/claude-code)

### Install

```bash
git clone https://github.com/kiyor/soul-cli.git
cd soul-cli
go build -ldflags "-X main.defaultAppName=myai" -o myai .
mv myai ~/go/bin/     # or anywhere in PATH
```

!!! tip "The name is everything"
    The `-X main.defaultAppName=myai` flag bakes the identity into the binary. **All paths, env vars, and logs are derived from this name:**

    | Binary name | Home dir | Env prefix | Data dir |
    |-------------|----------|------------|----------|
    | `myai` | `~/.openclaw/` | `MYAI_` | `~/.openclaw/data/` |
    | `jarvis` | `~/.openclaw/` | `JARVIS_` | `~/.openclaw/data/` |
    | `atlas` | `~/.openclaw/` | `ATLAS_` | `~/.openclaw/data/` |

    Want a completely separate home? Set `MYAI_HOME=~/my-ai`.

### Create Workspace

```bash
mkdir -p ~/.openclaw/workspace/memory
```

### Write Soul Files

Create these in `~/.openclaw/workspace/`:

=== "SOUL.md (required)"

    ```markdown
    # SOUL.md

    ## Personality
    - Direct and concise — lead with the answer, not the reasoning
    - Reliable — do the work silently, report when done

    ## Principles
    - Bias toward action: if it's safe and reversible, just do it
    - Ask before destructive operations
    - Say "I don't know" when you don't

    ## Speaking Style
    - No filler words, no "certainly!", no "great question!"
    - Code speaks louder than explanations
    ```

=== "IDENTITY.md (required)"

    ```markdown
    # IDENTITY.md

    - **Name:** MyAI
    - **Role:** Personal engineering assistant
    ```

=== "USER.md (recommended)"

    ```markdown
    # USER.md

    - **Name:** Alex
    - **Timezone:** America/Los_Angeles
    - **Preferences:** Go, Docker, concise answers
    ```

!!! info "These are just markdown"
    No schema, no DSL. Write whatever you want. See [Soul Files Guide](guides/soul-files.md) for all available files.

### Launch

```bash
myai
```

That's it. Claude Code starts with your soul injected. Ask it _"What's your name?"_ — it knows.

## For Multi-Agent Users

If you're running multiple agents (e.g. via [OpenClaw](https://github.com/nicepkg/openclaw)):

```bash
# Build one binary per agent
go build -ldflags "-X main.defaultAppName=main"     -o main .
go build -ldflags "-X main.defaultAppName=sentinel"  -o sentinel .
```

Each binary auto-discovers its agent config from `openclaw.json` by matching the binary name to `agents.list[].id`. The only things you decide:

1. **Name** — the `-X main.defaultAppName=xxx` flag
2. **Workspace** — defaults to `~/.openclaw/workspace`, override with `<NAME>_HOME` env var

Everything else (paths, locks, databases, logs) is derived automatically.

See [Multi-Agent Guide](guides/multi-agent.md) for details.

## What's Next

| Want to... | Read... |
|------------|---------|
| Understand how it all fits together | [Core Concepts](concepts.md) |
| Customize soul files in depth | [Soul Files Guide](guides/soul-files.md) |
| Set up daily memory & evolution | [Automation Guide](guides/automation.md) |
| Run a persistent server with Web UI | [Server Mode Guide](guides/server.md) |
| See every CLI command | [CLI Reference](reference/cli.md) |
