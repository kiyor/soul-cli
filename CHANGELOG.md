# Changelog

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
