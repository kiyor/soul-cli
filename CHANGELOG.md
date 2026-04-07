# Changelog

## v1.8.0

### Session Categories & Lifecycle

- **Session categories**: `interactive`, `heartbeat`, `cron`, `evolve` — ephemeral categories (heartbeat/cron/evolve) auto-destroy on completion, don't count toward maxSessions
- **Session tags**: Freeform labels for filtering, stored in DB as JSON array
- **Category filter API**: `GET /api/sessions?category=interactive` filters session list
- **DB migrations**: Added `chrome_enabled`, `gal_id`, `category`, `claude_session_id`, `tags` columns (idempotent ALTER TABLE)

### GAL (Visual Novel) System

- **GAL session support**: `gal_id` field on session create/snapshot, links sessions to GAL save files
- **GAL replay mode**: `skip_replay` WS flag lets frontend handle history display for muted replay styling
- **GAL context injection**: `galContext` global for injecting save JSON into prompt

### Chrome Remote Debugging

- **`--chrome` flag**: Pass-through to Claude Code for Playwright browser control
- **Runtime chrome toggle**: `POST /api/sessions/{id}/chrome` reloads process with `--chrome` (suppressClose prevents UI disruption)
- **Chrome flag persistence**: `ChromeEnabled` field on session struct, passed through on resume/reload

### Prompt System

- **`weiran prompt`**: New subcommand prints full assembled prompt to stdout with section stats on stderr
- **`weiran lint`**: Validates markdown frontmatter formats across topics, skills, and CLAUDE.md files
- **CORE.md loading**: Read-only rules file loaded before soul files, auto-restored if modified
- **Feedback auto-injection**: Scans `memory/topics/feedback_*.md`, extracts frontmatter name+description, injects as behavioral rules section
- **Dynamic content boundary**: Explicit split between static (soul/identity/tools) and dynamic (daily notes/TG/sessions) prompt sections, mirroring Claude Code's prompt caching concept
- **Launch directory capture**: Records original CWD before chdir, available for context
- **Current time injection**: Agent prompt includes `Current time` with time + day of week

### Safety & Hooks

- **CORE.md integrity guard**: Safety check auto-restores CORE.md from git HEAD if modified
- **Soul file shrinkage detection**: Warns if any protected file (SOUL/IDENTITY/USER/AGENTS/BOOT) shrinks >20% — prevents accidental content deletion during "optimization"
- **Markdown format validation**: `validateMdFormats()` checks topic/skill frontmatter and CLAUDE.md structure

### Server Mode

- **Haiku naming pool**: `server_haiku_pool.go` — pool of Haiku instances for fast session auto-naming
- **Session create refactor**: `createSessionWithOpts` replaces positional args with `sessionCreateOpts` struct
- **Control response routing**: `bridgeStdout` routes `control_response` messages to sync waiters via `deliverResponse`
- **Suppress close on reload**: `suppressClose` atomic bool prevents "Session ended" on intentional process restart (chrome toggle)
- **Resume flag passthrough**: `-r` TUI picker now passes through extra flags (e.g. `--chrome`) to the resumed session

### Web UI

- **GAL interactive components**: Choice cards, quick reply chips, star rating, image gallery — all rendered from markdown code fences (`weiran-choices`, `weiran-chips`, `weiran-rating`, `weiran-gallery`)
- **GAL replay styling**: Muted left-border + "回放" label for replayed history messages
- **Category chip filter**: Session list filterable by category chips
- **Model badge repositioning**: Moved to accommodate hamburger menu button

### Code Quality

- **Project roots**: `workspace/scripts` added to default project scan roots
- Removed unused `os/exec` import from `server_rename.go`

## v1.7.0

### Server Mode

- **Auto-rename API**: New `POST /api/sessions/{id}/auto-rename` endpoint — calls Claude CLI (`claude -p`) instead of direct Anthropic API, uses system model routing
- **User message broadcasting**: User messages now broadcast to SSE/WS subscribers, persisting across session switches
- **Resume dedup**: Resuming a Claude session that's already active returns the existing session instead of creating a duplicate
- **Resume with display name**: `POST /api/resume` accepts `name` field to preserve original session title
- **Activity tracking**: Broadcaster tracks `lastEventTime`, exposes `last_event` and `idle_seconds` in session snapshot
- **Session name lookup fix**: `readClaudeSessionName` now scans all session JSON files by `sessionId` field instead of assuming filename = UUID
- **Nested env cleanup**: `filterNestedClaudeEnv` strips `CLAUDE_CODE_SESSION` / `CLAUDE_CODE_ENTRY_POINT` from child processes

### Web UI

- User messages rendered from broadcast events (persist when switching sessions)
- `/rename` slash command: with args = manual rename, no args = auto-rename via Haiku
- History sessions already active → click selects instead of re-resuming
- Session switch cleanup (typewriter state, thinking indicator reset)
- `doResume()` refactor for direct history-to-session navigation

### Evolve

- E2E server API test (`tests/server-api-e2e.sh`) integrated into evolve workflow

### Code Quality

- Go formatting cleanup (string concatenation, struct field alignment)
- Removed direct Anthropic API dependency from auto-rename (`getAnthropicAPIKey` deleted)
- Updated tests for new `filterNestedClaudeEnv` and CLI-based rename

## v1.6.0

### Server Mode Enhancements

- **Server mode context injection**: Boot protocol now detects server mode and injects Web UI-specific environment instructions (image rendering, link previews, tool chain visibility)
- **OG link preview API**: New `GET /api/link-preview?url=` endpoint fetches Open Graph tags for URL preview cards in Web UI
- **Session name sync from Claude Code**: Auto-reads Claude Code's session metadata on init to sync session names (replaces generic `session-*` / `resume-*` prefixes)
- **Image support in history**: Parse and display base64 images from tool_result content blocks when loading session history
- **bridgeStdout onInit callback**: Split stdout bridge into `onInit` + `onResult` callbacks for cleaner session lifecycle handling

### UI

- Mobile-first Web UI improvements (from previous commits, continued iteration)

## v1.5.0

- Session rename, slash commands, typewriter streaming, launchd integration

## v1.4.0

- Mobile-first UI overhaul
- Historical message loading on session resume

## v1.3.0

- Server mode — HTTP API + web UI for persistent Claude Code sessions
- Refactored all task prompts to text/template

## v1.2.0

- Open-sourced as soul-cli
- Version management with build/rollback
- Evolve mode for daily self-iteration
