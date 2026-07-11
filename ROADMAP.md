# Roadmap

What is known to be missing or deferred, relative to the design document
(`spec.md`, untracked) and to gaps found while using the relay. Items move to
[CHANGELOG.md](CHANGELOG.md) when they ship.

## Near term (correctness & auditability)

- [x] **Log agentic requests individually.** Every authorized agentic request
  now emits an Info audit line (request-id + path); denials are logged at
  Warn with a reason.
- [x] **Handle `max_tokens` honestly.** Backends declare enforcement via
  `Capabilities.MaxTokens`; a one-time warning is logged when clients set it
  on a non-enforcing backend; documented in `docs/api.md`;
  `max_completion_tokens` now decoded on the OpenAI endpoint.
- [x] **Back-port resolved decisions into `spec.md`.** DQ-1/2/4 answers,
  REQ-EXEC-04/06 mechanisms, structured content, error surfacing, and the
  process-group kill correction folded into the design doc.
- [x] **Requirements document: declared lost.** `agent-relay-spec.md`
  (requirements v0.1), from which `spec.md` derives, is not recoverable.
  Decision: `spec.md` is now the root authoritative document, and the
  REQ-IDs it cites are defined by its own prose rather than audited against
  an upstream file.

## Undecided (needs a design decision first)

- [x] **Config file overlay (DQ-3): resolved as env-only.** A TOML overlay
  adds surface without need for this project; the spec now declares
  env-first, env-only as final.
- [x] **Retrieving agentic outputs.** Implemented as a retrieval endpoint:
  agentic requests sent with `X-Agentic-Keep-Outputs: true` retain their
  ephemeral working directory under an unguessable id (returned in the
  `X-Agentic-Outputs` response header); `GET /v1/outputs/{id}` lists files,
  `GET /v1/outputs/{id}/files/{path}` downloads, `DELETE /v1/outputs/{id}`
  releases; retained outputs are swept after `RELAY_OUTPUTS_TTL`.

## Later (v0.2+ candidates)

- [x] **Structured content (wire level).** `tool_use`/`tool_result` blocks,
  `tools[]`, and `tool_choice` now decode on both wire formats; responses can
  carry `tool_use` blocks with streaming `input_json_delta`; the claude
  backend flattens structured history into its transcript.
- [x] **Vision/PDF bridge.** Base64 `image`/`document` blocks are decoded
  into a per-request ephemeral working directory and viewed via the CLI's
  read-only Read tool (auto-allowed within its cwd). Model-mediated rather
  than structurally guaranteed; 20 MiB/block; base64 sources only.
- [ ] **Client-tool execution.** Even with the wire support above, the
  claude CLI has no raw tool-calling mode (it runs its own agent loop), so
  requests with `tools[]` are rejected with 400 on the claude backend and
  agentic clients (Claude Agent SDK, Claude Code) still cannot use the relay
  as their backend. Unblocking this requires either an upstream CLI feature
  or a backend that fronts the raw model API.
- [ ] **Second backend (Gemini or Codex).** The registry seam exists
  (REQ-BK-03) but is unexercised; a second adapter would prove it.
- [ ] **Model-map routing across backends.** When a second backend lands,
  route requests by logical model name instead of the global
  `RELAY_BACKEND` selection (resolves DQ-2 fully).
- [ ] **NixOS packaging.** `docs/deployment.md` sketches `buildGoModule` +
  a systemd module; nothing declarative is in-tree.
- [ ] **Native multi-turn conversations.** History is currently flattened
  to a `Human:`/`Assistant:` transcript on stdin — an approximation the CLI
  imposes.

## Harness engineering

Turning the relay from "an inference proxy that can also run a throwaway
agent" into an *observable, resumable agent-execution service* — the
substrate a harness needs. Grounded in what the CLI's `stream-json` already
emits and the relay currently drops.

- [x] **Tool-activity traces.** The CLI's own tool calls and results are
  parsed from its `assistant`/`user` lines and surfaced two ways: opt-in SSE
  events (`X-Agent-Traces: true` → `agent_tool_use` / `agent_tool_result`,
  off by default so strict SDKs are unaffected) and a `trace.jsonl` written
  into retained output directories.
- [ ] **Session continuity (`--resume`).** Every stream-json line carries a
  `session_id` that the relay discards. Expose it (`X-Session-Id` response
  header), accept it back on a later request, and pass `--resume` to the CLI:
  the agent keeps its context and prompt cache across requests. Composes with
  `X-Agentic-Keep-Outputs` to give a persistent agentic workspace; likely
  requires reusing the retained working directory.
- [x] **Per-request cost accounting.** The CLI's reported `total_cost_usd`
  and token counts now ride on `EventMessageStop`; every served request logs
  a `request usage` line (tokens + `cost_usd`, correlated by `X-Request-Id`)
  and feeds `input_tokens_total` / `output_tokens_total` / `cost_usd_total`
  in `/v1/metrics`.
- [ ] **Backpressure signals.** A full pool answers a bare 503; add
  `Retry-After`, and a per-token quota (SECURITY.md lists per-caller rate
  limiting as explicitly not defended).
- [ ] **Per-request timeout override.** An `X-Request-Timeout` header capped
  by `RELAY_REQUEST_TIMEOUT`: a long agentic task and a short classification
  should not share one global deadline.

## Wire-compatibility polish

- [x] Signal (rather than silently drop) unsupported sampling parameters:
  decoded on both wires, backends declare `Capabilities.Sampling`, and the
  relay logs a one-time warning naming the dropped parameters.
- [x] Include usage in OpenAI streaming responses
  (`stream_options: {"include_usage": true}`): a final empty-choices chunk
  carries usage before `[DONE]`.
- [x] `EventUsage` removed; usage now rides on `EventMessageStart` (input
  tokens, as the wire formats report them up front) and `EventMessageStop`.
  Anthropic `message_start` now reports real `input_tokens` instead of zero.

## Non-goals (deliberate)

- **TLS termination.** The relay targets loopback and trusted private
  networks (Tailscale); put a reverse proxy in front if you need TLS.
- **Multi-tenant service.** One operator, one subscription, personal use —
  see the [terms-of-service disclaimer](README.md#disclaimer--terms-of-service).
