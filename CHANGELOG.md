# Changelog

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
