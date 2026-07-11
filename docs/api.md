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

- `content` may be a string or an array of content blocks. Supported block
  types: `text`, `tool_use` (assistant turns), `tool_result` (user turns),
  and base64 `image`/`document` blocks (see "Attachments" below).
  `thinking` blocks echoed back by clients are dropped silently; unknown
  block types are rejected with 400.
- Roles are limited to `user` and `assistant`.
- `max_tokens` is accepted for wire compatibility (the Anthropic format makes
  it mandatory) but **not enforced** by the claude backend — the CLI has no
  flag to cap output tokens, so responses may exceed it. The relay logs a
  one-time warning when a request carries `max_tokens` on a backend that
  cannot enforce it.
- `tools` and `tool_choice` are decoded, but serving them requires a backend
  that supports client-defined tools — see "Client-defined tools" below.
- `temperature`, `top_p`, `top_k`, and `stop_sequences` are decoded but
  **ignored** by the claude backend (the CLI has no such flags). The relay
  logs a one-time warning naming the parameters it dropped, rather than
  ignoring them silently.

**Streaming** (`"stream": true`) returns `text/event-stream` with the
standard Anthropic event sequence, flushed per event: `message_start`,
`content_block_start`, `content_block_delta` (text deltas),
`content_block_stop`, `message_delta` (stop reason + usage), `message_stop`.
Backend failures mid-stream are delivered as an `error` event.

**Non-streaming** returns a single `message` object with text content blocks
and token usage. When a backend emits tool calls, responses carry `tool_use`
blocks (`content_block_start` + `input_json_delta` when streaming) and
`stop_reason: "tool_use"`.

### Attachments (images and PDFs)

Standard base64 `image` and `document` blocks are accepted — the same shape
Anthropic SDK clients already send:

```json
{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "<base64>"}}
{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "<base64>"}}
```

Accepted media types: `image/png`, `image/jpeg`, `image/gif`, `image/webp`,
`application/pdf`; 20 MiB decoded per block; only `base64` sources (no URL
fetching). On the claude backend this works as a **bridge**: the relay
decodes each attachment into a per-request ephemeral directory, runs the CLI
with that directory as its working directory, and replaces the block with a
text reference that the CLI's read-only Read tool follows to view the file.
The directory is deleted when the request ends.

Two consequences of the bridge design: viewing is *model-mediated* (the model
follows the reference; in practice it does, but it is not the structural
guarantee of native API vision), and a request carrying attachments runs in a
clean ephemeral directory — it does not see the relay's own working
directory.

### Client-defined tools

The wire format fully supports structured content: `tool_use`/`tool_result`
blocks in conversation *history* are accepted on any backend (the claude
backend flattens them into its text transcript). However, a request carrying
`tools[]` — asking the model to call the *caller's* tools and stop — is
rejected with 400 unless the backend reports client-tool support.

The claude CLI backend does **not**: the CLI runs its own agent loop with its
own tools and has no raw tool-calling mode. In practice this means agentic
clients (the Claude Agent SDK, Claude Code) still cannot use the relay as
their backend; classic chat clients are unaffected. For the full comparison
of what the API offers that the relay cannot, see
[API vs relay limitations](limitations.md).

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
`max_tokens` and `max_completion_tokens` (the modern OpenAI parameter, which
takes precedence) are optional here and carry the same limitation as on
`/v1/messages`: accepted, but not enforced by the claude backend. Sampling
parameters (`temperature`, `top_p`, `stop`) are likewise decoded but ignored,
with a one-time warning.

Streaming returns `chat.completion.chunk` SSE frames terminated by
`data: [DONE]`. With `stream_options: {"include_usage": true}`, a final chunk
with an empty `choices` array carries `usage` just before `[DONE]`, as the
OpenAI API does. Non-streaming returns a `chat.completion` object with
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

### Session continuity (resuming a conversation)

Every response carries the backend's conversation id:

```
X-Session-Id: 133cc414-ce5e-4b5a-80ca-3997e1ce9641
```

Send it back on a later request to **resume** that conversation: the backend
keeps its context (and its prompt cache) instead of starting fresh, and the
relay does not have to replay the history as a flattened transcript.

```sh
# turn 1 — note the X-Session-Id in the response headers
curl -D- http://127.0.0.1:18082/v1/messages -H "x-api-key: $TOKEN" \
  -d '{"model":"haiku","max_tokens":100,"messages":[{"role":"user","content":"Remember: ANANAS."}]}'

# turn 2 — resume
curl http://127.0.0.1:18082/v1/messages -H "x-api-key: $TOKEN" \
  -H "X-Session-Id: 133cc414-…" \
  -d '{"model":"haiku","max_tokens":100,"messages":[{"role":"user","content":"Which word?"}]}'
# → "ANANAS"
```

**The workspace must be stable.** The claude CLI keys its sessions by working
directory, so resuming only works where that directory persists:

| Mode | Resumable? |
|---|---|
| Inference | ✅ — the workdir is the static `RELAY_CLAUDE_WORKDIR` |
| Agentic with a retained workspace | ✅ — pin it by echoing the previous `X-Agentic-Outputs` id back on the request |
| Agentic with an ephemeral workspace | ❌ — 400, with an explanation |

Pinning a workspace *and* resuming a session is the combination that gives a
**persistent agentic workspace**: the agent keeps both its files and its
memory across requests.

```sh
# turn 2, agentic: same files, same conversation
curl http://127.0.0.1:18082/v1/messages -H "x-api-key: $TOKEN" \
  -H "X-Agentic-Authorization: Bearer $AGENTIC" \
  -H "X-Agentic-Outputs: 668e8c35…" \
  -H "X-Session-Id: 32e9fa00-…" \
  -d '{"model":"haiku","max_tokens":100,"messages":[{"role":"user","content":"What file did you create?"}]}'
```

Session ids are validated as UUIDs before reaching the CLI (a caller-supplied
argv element must not be able to become a flag). Note that only the *new*
message needs to be sent on a resumed turn — the backend already holds the
history.

### Agent tool traces

An agentic run is otherwise a black box: the client sees text, never what the
agent *did*. Two ways to observe it:

**Live, on the stream** — send `X-Agent-Traces: true` on a streaming
`/v1/messages` request. The relay then emits two extra SSE event types
alongside the standard ones:

```
event: agent_tool_use
data: {"type":"agent_tool_use","id":"toolu_…","name":"Write","input":{…}}

event: agent_tool_result
data: {"type":"agent_tool_result","tool_use_id":"toolu_…","content":"File created…","is_error":false}
```

Traces are **opt-in** because unknown SSE event types can trip strict SDK
stream parsers; without the header the stream is byte-for-byte the standard
one. They carry no content-block indices, so they never disturb the normal
`content_block_*` sequence. Tool results are truncated to 4 KiB.

**Durably, as a file** — any request whose outputs are retained
(`X-Agentic-Keep-Outputs`) also gets a `trace.jsonl` in its output directory,
one JSON object per tool call and result, retrievable through the endpoints
below. No header needed, and no file is created if the agent used no tools.

Traces are available on `/v1/messages`; the OpenAI wire has no event-name
channel, so use the `trace.jsonl` route there.

### Retrieving agentic outputs

By default an agentic request's working directory is deleted when the
request ends. Send `X-Agentic-Keep-Outputs: true` on an agentic-authorized
request to retain it: the response carries an unguessable id in the
`X-Agentic-Outputs` header, usable with:

| Method + path | Effect |
|---|---|
| `GET /v1/outputs/{id}` | JSON listing (`{"id", "files":[{"path","size"}]}`) |
| `GET /v1/outputs/{id}/files/{path}` | Download one artifact (octet-stream) |
| `DELETE /v1/outputs/{id}` | Release immediately (204) |

All three require the normal caller credential. Retained outputs are swept
after `RELAY_OUTPUTS_TTL` (default 10m); the header on a non-agentic request
is a 400. Path traversal in `{path}` is refused.

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
  "agentic_denied": 0,
  "backend_errors": 0,
  "input_tokens_total": 1280,
  "output_tokens_total": 9450,
  "cost_usd_total": 0.284
}
```

`cost_usd_total` is the sum of the dollar costs the backend reports per turn
(the claude CLI reports one). Each served request also logs a `request usage`
line carrying its `input_tokens`, `output_tokens`, and `cost_usd`, correlated
by `X-Request-Id` — enough for a fanning-out client to attribute spend
per request.

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
