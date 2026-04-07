# Server API Reference

All endpoints require `Authorization: Bearer <token>` header unless noted.

Base URL: `http://<host>:<port>`

## Health

### `GET /api/health`

Health check. **No authentication required.**

```bash
curl http://localhost:9847/api/health
```

```json
{ "status": "ok" }
```

## Sessions

### `POST /api/sessions`

Create a new Claude Code session.

```bash
curl -X POST http://localhost:9847/api/sessions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "fix-auth-bug",
    "initial_message": "Look at the auth middleware",
    "soul_files": true
  }'
```

**Request body:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | auto-generated | Session display name |
| `initial_message` | string | — | First message to send |
| `soul_files` | bool | `true` | Inject soul prompt |

**Response:** `201 Created`

```json
{
  "id": "abc123",
  "name": "fix-auth-bug",
  "status": "active",
  "created_at": "2025-04-07T10:30:00Z"
}
```

### `GET /api/sessions`

List active sessions.

```bash
curl http://localhost:9847/api/sessions \
  -H "Authorization: Bearer $TOKEN"
```

**Query parameters:**

| Param | Description |
|-------|-------------|
| `category` | Filter by category: `interactive`, `cron`, `heartbeat`, `evolve` |

**Response:** `200 OK`

```json
[
  {
    "id": "abc123",
    "name": "fix-auth-bug",
    "status": "active",
    "created_at": "2025-04-07T10:30:00Z",
    "last_activity": "2025-04-07T10:35:00Z"
  }
]
```

### `GET /api/sessions/:id`

Get session details.

```bash
curl http://localhost:9847/api/sessions/abc123 \
  -H "Authorization: Bearer $TOKEN"
```

### `DELETE /api/sessions/:id`

Destroy a session (kills Claude Code process).

```bash
curl -X DELETE http://localhost:9847/api/sessions/abc123 \
  -H "Authorization: Bearer $TOKEN"
```

**Response:** `204 No Content`

## Messages

### `POST /api/sessions/:id/message`

Send a message to a session.

```bash
curl -X POST http://localhost:9847/api/sessions/abc123/message \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message": "now write a test for the fix"}'
```

**Request body:**

| Field | Type | Description |
|-------|------|-------------|
| `message` | string | The message to send |

**Response:** `200 OK`

```json
{ "status": "sent" }
```

### `GET /api/sessions/:id/stream`

Server-Sent Events (SSE) stream of session output.

```bash
curl -N "http://localhost:9847/api/sessions/abc123/stream?token=$TOKEN"
```

!!! note "Authentication"
    SSE endpoints accept the token as a query parameter (`?token=xxx`) since EventSource doesn't support custom headers.

**Event types:**

| Event | Data | Description |
|-------|------|-------------|
| `message` | JSON | Claude's response content |
| `tool_use` | JSON | Tool invocation |
| `tool_result` | JSON | Tool output |
| `error` | JSON | Error occurred |
| `done` | — | Session finished responding |

## Control

### `POST /api/sessions/:id/control`

Control a running session.

```bash
# Interrupt current operation
curl -X POST http://localhost:9847/api/sessions/abc123/control \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"action": "interrupt"}'

# Change model
curl -X POST http://localhost:9847/api/sessions/abc123/control \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"action": "set_model", "model": "claude-sonnet-4-20250514"}'
```

**Actions:**

| Action | Parameters | Description |
|--------|------------|-------------|
| `interrupt` | — | Cancel current Claude operation |
| `set_model` | `model` | Change the Claude model |

### `POST /api/sessions/:id/chrome`

Toggle Chrome automation for a session.

```bash
curl -X POST http://localhost:9847/api/sessions/abc123/chrome \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"enabled": true}'
```

!!! warning
    Toggling Chrome restarts the Claude Code subprocess. The session context is preserved, but there may be a brief interruption.

## History

### `GET /api/history`

List historical sessions available for resume.

```bash
curl "http://localhost:9847/api/history?q=kubernetes" \
  -H "Authorization: Bearer $TOKEN"
```

**Query parameters:**

| Param | Description |
|-------|-------------|
| `q` | Search query (fuzzy match on name/content) |
| `limit` | Max results (default: 50) |

### `POST /api/sessions/resume`

Resume a historical session.

```bash
curl -X POST http://localhost:9847/api/sessions/resume \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"session_id": "abc123"}'
```

## Configuration

### `GET /api/config`

Get server configuration (non-sensitive fields).

```bash
curl http://localhost:9847/api/config \
  -H "Authorization: Bearer $TOKEN"
```
