# HTTP API

## Authentication

Every endpoint except `GET /health` requires a configured token, supplied as
either header:

```
Authorization: Bearer <token>
x-api-key: <token>
```

Tokens are compared in constant time. Unauthenticated requests are rejected
with 401 **before** any backend subprocess is spawned. When no tokens are
configured (only permitted on loopback binds), all callers pass.

## `POST /v1/messages` — Anthropic Messages

Request body (v1 supports text content only):

```json
{
  "model": "sonnet",
  "max_tokens": 1024,
  "system": "optional system prompt (string or text blocks)",
  "stream": true,
  "messages": [
    {"role": "user", "content": "hello"}
  ]
}
```

- `content` may be a string or an array of `{"type":"text","text":...}`
  blocks; other block types are rejected with 400.
- Roles are limited to `user` and `assistant`.

**Streaming** (`"stream": true`) returns `text/event-stream` with the
standard Anthropic event sequence, flushed per event: `message_start`,
`content_block_start`, `content_block_delta` (text deltas),
`content_block_stop`, `message_delta` (stop reason + usage), `message_stop`.
Backend failures mid-stream are delivered as an `error` event.

**Non-streaming** returns a single `message` object with one text content
block and token usage.

## `POST /v1/chat/completions` — OpenAI Chat Completions

```json
{
  "model": "sonnet",
  "stream": false,
  "messages": [
    {"role": "system", "content": "optional"},
    {"role": "user", "content": "hello"}
  ]
}
```

`system` / `developer` messages map onto the backend system prompt.
Streaming returns `chat.completion.chunk` SSE frames terminated by
`data: [DONE]`; non-streaming returns a `chat.completion` object with
`usage`.

## Agentic requests

When the relay runs with `RELAY_AGENTIC_ENABLED=true` and
`RELAY_AGENTIC_PER_REQUEST_AUTHZ=true`, agentic execution is granted **per
request**: in addition to the normal caller credential, the request must
carry a valid agentic credential from `RELAY_AGENTIC_TOKENS`:

```
X-Agentic-Authorization: Bearer <agentic-token>
```

- Without the header, the request is served in plain inference mode (no
  permission flags, no side effects).
- With an invalid credential — including a caller token, the two sets are
  never interchangeable — the request is rejected with **403** before any
  subprocess is spawned.
- If the header is sent to a relay whose agentic mode is disabled, the
  response is also 403.

Authorized agentic requests run with the operator-configured permission
flags, each in its own ephemeral working directory. See
[execution-modes.md](execution-modes.md) for the full inference-vs-agentic
comparison.

## `GET /health`

Unauthenticated liveness probe: `{"status":"ok"}`.

## `GET /v1/metrics`

Authenticated, minimal JSON counters:

```json
{
  "uptime_seconds": 120,
  "requests_total": 42,
  "in_flight": 1,
  "rejected_busy": 0,
  "unauthorized": 3,
  "backend_errors": 0
}
```

## Errors

| Status | Meaning |
|---|---|
| 400 | Malformed body, unsupported role or content block type. |
| 401 | Missing or invalid credential. |
| 502 | Backend failed before producing a stream. |
| 503 | All concurrency slots busy; retry later. No subprocess was spawned. |

Error bodies follow the wire format of the endpoint (Anthropic
`{"type":"error","error":{...}}` shape on `/v1/messages`, OpenAI
`{"error":{...}}` shape on `/v1/chat/completions`).

Every response carries an `X-Request-Id` header for log correlation.
