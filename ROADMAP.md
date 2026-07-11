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
- [x] **Client-tool execution.** Solved via MCP, not a raw-model mode: the
  relay hosts an MCP server exposing the caller's tools and parks the CLI
  subprocess between turns, so the standard Messages API tool loop (and the
  official SDKs) work unmodified. Remaining: `tool_choice` is not enforced,
  and a parked conversation holds a concurrency slot.
- [x] **Second backend: Ollama.** A local-model backend of a deliberately
  different kind (HTTP client, not a subprocess), which proves the registry
  seam (REQ-BK-03): it touched neither the wire adapters nor the core. It
  honors `max_tokens` and sampling (which the CLI cannot) and calls client
  tools natively — on models that support them.
- [x] **Model-map routing across backends (DQ-2 resolved).**
  `RELAY_MODEL_ROUTES` sends a logical model to a backend; unrouted models go
  to `RELAY_BACKEND`; a route to an unknown backend refuses to start.
  Capabilities are now resolved **per request**, from the backend that will
  actually serve it.
- [ ] **Antigravity CLI backend (`agy`) — conditional.** Probed at v1.1.1: it
  is a genuinely *new token source* (Gemini 3.5 / 3.1 Pro, **Claude Sonnet and
  Opus 4.6**, GPT-OSS 120B — on the Antigravity quota, not your Anthropic
  subscription), and it supports MCP (`mcpServers` appears in its config
  surface). But `agy -p` prints **plain buffered text**: no `--output-format`
  (no hidden flag either — the binary was checked), no streaming (a ten-line
  answer arrives in a single instant), no usage, no cost, no session id, no
  tool traces, no structured tool calls. A backend today would report zeros
  for every counter, would have to fake SSE by emitting the whole answer at
  once, and could not reuse the MCP tool bridge — nothing would correlate a
  parked call with its conversation. It would be the most degraded backend
  here, adding exactly one of the five columns that matter.

  **Condition to revisit:** Antigravity exposing a structured output mode
  (streaming + usage), as the claude CLI does with `--output-format
  stream-json`. It is a young CLI that visibly copies claude's conventions, so
  this is plausible; on that day the adapter becomes trivial and immediately
  worthwhile.

  *Interim option, if wanted:* a text-only backend (~120 lines) would already
  serve batch work where neither streaming nor accounting matters
  (summarization, classification, translation) — at the cost of a third
  special case in the capability matrix.
- [ ] **NixOS packaging.** `docs/deployment.md` sketches `buildGoModule` +
  a systemd module; nothing declarative is in-tree.
- [ ] **Native multi-turn conversations.** History is currently flattened
  to a `Human:`/`Assistant:` transcript on stdin — an approximation the CLI
  imposes. (Largely superseded by session continuity: `X-Session-Id` lets the
  backend keep its own conversation instead of replaying a transcript.)

### The rule for adding a backend

A backend earns its place when it brings a **new source of tokens** you
already own, or a **new class of capability** the current ones lack. Not to
"prove the architecture" — Ollama did that. Every adapter is permanent
surface: CLI drift, tests, docs, one more column in the capability matrix.

By that rule the strongest remaining candidate is a **raw-API backend**
(Anthropic- or OpenAI-compatible HTTP): it would unlock the whole
"structurally impossible" list at once — guaranteed structured outputs, exact
replay and prefill, prompt-cache control, enforced `tool_choice`,
thinking/effort, `count_tokens`, Batches — at the cost of API billing. Model
routing already makes it composable: `sonnet` on the subscription for
everyday work, `sonnet-strict` on the API for jobs that need guarantees.

Agent-CLI wrappers (OpenCode, Aider, Goose, Cline) fail the rule: they are
agents *on top of* providers, so they add a process and their own opinions
(system prompt, tools, agent loop) while inheriting the very limitations we
already fight — and if they authenticate against the same subscription, they
are strictly redundant with the `claude` backend.

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
- [x] **Session continuity (`--resume`).** The backend conversation id is
  returned as `X-Session-Id` and accepted back to resume; retained outputs
  can be pinned with `X-Agentic-Outputs` to keep the workspace stable
  (the CLI keys sessions by working directory), which together give a
  persistent agentic workspace — files *and* memory. Resuming without a
  stable workspace is refused with an explanatory 400.
- [x] **Per-request cost accounting.** The CLI's reported `total_cost_usd`
  and token counts now ride on `EventMessageStop`; every served request logs
  a `request usage` line (tokens + `cost_usd`, correlated by `X-Request-Id`)
  and feeds `input_tokens_total` / `output_tokens_total` / `cost_usd_total`
  in `/v1/metrics`.
- [x] **Backpressure signals.** 503 (busy) and 429 (quota) both carry
  `Retry-After`; `RELAY_RATE_LIMIT_RPM` adds a per-caller token bucket
  (off by default), counted in `/v1/metrics` as `rate_limited`. Remaining
  ideas: a token/cost budget rather than a request-rate quota, and shared
  state across replicas.
- [x] **Per-request timeout override.** `X-Request-Timeout` sets this
  request's deadline, clamped by `RELAY_REQUEST_TIMEOUT` (now both default
  and ceiling) and echoed back as applied. An expired deadline answers 504
  (not 502), so a client can tell it apart from a backend failure.

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
