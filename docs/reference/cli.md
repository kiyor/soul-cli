# CLI Reference

## Synopsis

```
myai [flags]
myai [command] [args]
```

## Modes

### Interactive (default)

```bash
myai
```

Launches Claude Code with soul prompt injected. The process replaces itself (`syscall.Exec`) — Claude gets your full terminal.

### One-Shot

```bash
myai -p "check disk usage and warn if above 80%"
```

Runs a single task with soul context, then exits.

### Resume

```bash
myai -r                 # TUI picker for recent sessions
myai -r abc123          # Resume specific session by ID
myai -r --chrome        # Resume with Chrome automation enabled
```

### Cron

```bash
myai --cron
```

Memory consolidation: scan recent sessions, update daily notes, extract patterns. See [Automation Guide](../guides/automation.md).

### Heartbeat

```bash
myai --heartbeat
```

Health check: monitor services, process tasks, detect anomalies. See [Automation Guide](../guides/automation.md).

### Evolve

```bash
myai --evolve
```

Self-improvement: review interactions, update soul files, fix bugs. See [Automation Guide](../guides/automation.md).

### Server

```bash
myai server [--host HOST] [--port PORT] [--token TOKEN]
```

Start the HTTP server with Web UI. See [Server Mode Guide](../guides/server.md).

## Commands

### `init`

First-run setup wizard — creates workspace, generates soul files, installs setup-guide skill.

```bash
myai init                          # interactive wizard with archetype selection
myai init --yes                    # use all defaults, no prompts
myai init --archetype companion    # use companion personality template
myai init --archetype engineer --name kuro --owner alex --tz America/New_York
myai init --force                  # overwrite existing files
```

| Flag | Description |
|------|-------------|
| `--archetype` | Personality template: `companion`, `engineer`, `steward`, `mentor` |
| `--name` | AI name (default: binary name) |
| `--role` | Role description |
| `--personality` | Comma-separated keywords |
| `--owner` | Owner's name (default: `$USER`) |
| `--tz` | Timezone (default: system timezone) |
| `--yes`, `-y` | Skip prompts, use defaults |
| `--force`, `-f` | Overwrite existing files |

Generated files include a `<!-- soul:day0 -->` marker that triggers automatic personality enrichment on first interactive session.

### `status`

Quick health check (doesn't launch Claude):

```bash
myai status
```

Shows: service health, Claude Code version, database stats, config validity.

### `doctor`

Deep diagnostics:

```bash
myai doctor
```

Shows: everything in `status` plus process list, disk usage, model endpoint checks, metrics anomalies, memory analysis.

### `config`

Display current configuration:

```bash
myai config
```

### `prompt`

Print the assembled soul prompt with per-section token stats:

```bash
myai prompt
```

### `log`

View daily notes:

```bash
myai log          # today's notes
myai log 1        # yesterday's notes
myai log 3        # 3 days ago
```

### `diff`

Show soul/memory changes since last commit:

```bash
myai diff
```

### `clean`

Clean up old temporary directories:

```bash
myai clean
```

### `lint`

Validate markdown file formats:

```bash
myai lint
```

### `notify`

Send a Telegram message:

```bash
myai notify "deployment complete"
myai notify-photo https://example.com/screenshot.png "Dashboard screenshot"
```

### `build`

Safe self-compilation with automatic rollback:

```bash
myai build
```

Steps: backup current binary → compile → run tests → deploy. Rolls back on failure.

### `update`

Pull latest source and rebuild:

```bash
myai update
```

Equivalent to `git pull && myai build`.

### `versions`

List saved binary versions:

```bash
myai versions
```

### `rollback`

Restore a previous binary version:

```bash
myai rollback       # rollback to previous version
myai rollback 2     # rollback 2 versions back
```

### `sessions` / `ss`

Interactive session browser (TUI):

```bash
myai sessions            # browse all sessions
myai ss                  # alias
myai ss kubernetes       # pre-filter by keyword
myai ss --chrome         # show chrome-enabled sessions
```

### `db`

Session database management:

```bash
myai db stats            # session counts
myai db search <keyword> # search session summaries
myai db pending          # sessions needing review
myai db gc               # clean up deleted sessions
myai db patterns         # list extracted patterns
myai db cultivate        # generate skills from mature patterns
myai db recall           # sessions pending summary import
myai db save-batch       # batch import pending summaries
```

## Flags

### Global Flags

| Flag | Description |
|------|-------------|
| `-p "task"` | One-shot mode: run task and exit |
| `-r [id]` | Resume a previous session |
| `--cron` | Run memory consolidation |
| `--heartbeat` | Run health check |
| `--evolve` | Run self-improvement |
| `--chrome` | Enable Chrome automation (passed to Claude Code) |

### Init Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--archetype` | *(custom)* | Personality template: companion, engineer, steward, mentor |
| `--name` | binary name | AI name |
| `--role` | "personal engineering assistant" | Role description |
| `--personality` | "direct, reliable, warm" | Comma-separated keywords |
| `--owner` | `$USER` | Owner name |
| `--tz` | system timezone | Timezone string |
| `--yes` / `-y` | — | Skip interactive prompts |
| `--force` / `-f` | — | Overwrite existing files |

### Passthrough Flags

Any flag soul-cli doesn't recognize is forwarded to Claude Code:

```bash
myai --chrome                    # forwarded: claude --chrome
myai -p "task" --verbose         # forwarded: claude --verbose
myai --model claude-sonnet-4-20250514   # forwarded: claude --model ...
```

### Server Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--host` | `127.0.0.1` | Bind address |
| `--port` | `9847` | Listen port |
| `--token` | — | Auth token (required) |
