# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.8.0] - 2026-07-11

### Added

- **Agent2Agent (A2A) protocol adapter** (`internal/api/a2a`), opt-in via
  `RELAY_A2A_ENABLED`. A third wire adapter next to the Anthropic and OpenAI
  ones — the relay becomes an A2A *agent*, not an A2A client: it does not call
  other agents, so this is not a step toward routing.
  - `GET /.well-known/agent-card.json` — the Agent Card, served **without
    auth** (discovery is what a card is for). It advertises the JSON-RPC
    binding, streaming, the models served, and the agentic skill only when
    agentic execution is enabled.
  - `POST /a2a` — JSON-RPC 2.0 (`SendMessage`, `SendStreamingMessage`,
    `GetTask`, `CancelTask`, `ListTasks`), behind the same bearer auth, rate
    limit, concurrency cap, timeout and cost accounting as every other
    inference endpoint.
  - **Tasks map onto agentic execution.** An A2A task carrying the agentic
    credential (`X-Agentic-Authorization`, as on the other wires) runs in a
    retained workspace, and every file the agent produced comes back as an
    artifact — a `url` part pointing at `/v1/outputs/{id}/files/{path}`, since
    A2A defines no download endpoint.
  - **`contextId` is memory *and* filesystem.** Echoing it on the next message
    resumes the backend session and reuses the same workspace, so a peer can
    ask the agent to extend a file it wrote in an earlier task. Neither the
    Anthropic nor the OpenAI wire can express that.
  - `CancelTask` cancels the dispatch context — for the `claude` backend, a
    process-group kill of the subprocess.
  - A backend failure is a `TASK_STATE_FAILED` task, not a JSON-RPC error: the
    error channel is reserved for protocol and authorization faults.
  - `url` parts in an inbound message are **refused** — fetching a
    peer-supplied URL would make the relay an SSRF primitive. Attachments ride
    as `raw` parts through the existing attachment bridge.
- `RELAY_A2A_ENABLED`, `RELAY_A2A_MODEL` (A2A carries no model field; peers may
  still set `message.metadata.model`) and `RELAY_PUBLIC_URL` (the origin peers
  reach the relay on — what the card advertises and what artifact URLs are
  built from). A2A on a non-loopback bind that still advertises a loopback
  `RELAY_PUBLIC_URL` refuses to start.
- `docs/a2a.md`, and an entry in `SECURITY.md`: the Agent Card is a deliberate,
  unauthenticated disclosure of what this host serves.
- `upstream-bugs.md`: two defects in the A2A specification's prose, which
  contradicts its own normative proto (the sample Agent Card uses `security`
  where the proto says `security_requirements`; the v1.0 migration guide invents
  `taskStatusUpdate`/`taskArtifactUpdate` event names and `Task.createdAt` /
  `Task.lastModified` fields that do not exist).

### Changed

- **The stdlib-only rule is now scoped, not absolute.** The A2A adapter depends
  on the official [`a2a-go`](https://github.com/a2aproject/a2a-go) SDK — the
  project's first and only third-party dependency (JSON-RPC binding only: no
  gRPC, no protobuf). A2A v1.0 was a breaking redesign of 0.3 and keeps moving;
  hand-rolling the wire would have meant our tests validated *our reading* of
  the spec rather than the spec — and the spec's prose is demonstrably wrong in
  places (see `upstream-bugs.md`). NFR-INSPECT-01 now reads: the core, the
  backends and the security path remain standard-library only.
- Agentic authorization is decided in one place (`authorizeAgenticCred`), shared
  by the Anthropic, OpenAI and A2A surfaces, so they cannot drift apart on the
  decision that matters most.
- Auth and rate-limit rejections are now rendered in the wire format of the
  endpoint that was called: a rejected A2A call reads as a JSON-RPC error, not
  as an Anthropic one.
- Go 1.25 is now the minimum (the SDK requires it).

## [0.7.0] - 2026-07-11

### Added

- **Second backend: `ollama`** (`internal/backend/ollama`) — a local Ollama
  server over HTTP, deliberately a different *kind* of adapter than the
  subprocess-based CLI, which exercises the registry seam (REQ-BK-03):
  adding it touched neither the wire adapters nor the core. It streams,
  honors `max_tokens` and the sampling parameters (which the CLI cannot),
  calls client-defined tools natively (on models that support them —
  `qwen3.5` does, `llama3` does not, and the server's refusal is surfaced to
  the caller), and sends images natively. `core.BackendConfig` gains
  `BaseURL` for HTTP backends.
- **Model-name routing (DQ-2 resolved).** `RELAY_MODEL_ROUTES`
  (`llama3=ollama,phi3=ollama`) sends a logical model to a specific backend;
  unrouted models go to `RELAY_BACKEND`. Clients stay backend-agnostic — they
  name a model, as against the real API. A route to an unknown backend
  refuses to start. Capabilities (`max_tokens`, sampling, client tools) are
  now resolved **per request** from the backend that will serve it, not
  frozen at startup.
- `docs/backends.md`: the two backends side by side, and why routing.
- Scope boundary recorded (ROADMAP non-goal + `docs/backends.md`):
  **agent-relay is not an LLM provider router.** Model-name routing exists to
  compose the sources you already own — a subscription reachable only through
  its CLI, and local compute — not to aggregate HTTP providers. A service that
  already has an API needs no backend here; put a real router (LiteLLM) in
  front and register the relay as one of its providers. The rule for adding a
  backend is tightened accordingly, and the Mistral `vibe` and Antigravity
  `agy` CLIs are recorded as probed and (conditionally) rejected.
- **Client-defined tool execution.** `tools[]` now works: the relay runs the
  standard Messages API tool loop (`stop_reason: "tool_use"` → the caller
  executes the tool → `tool_result`), so the official SDKs work unmodified.
  The claude CLI has no raw tool-calling mode, so the relay bridges over
  **MCP**: a new `internal/toolbridge` hosts an MCP server (on its own
  loopback socket, with an unguessable session id and bearer token) exposing
  the caller's tools, and the CLI is pointed at it with `--mcp-config` plus
  an `--allowedTools` allowlist limited to those tools — its own Write/Bash
  stay unpermitted, so a tool request remains inference-mode. When the model
  calls a tool, the MCP handler *parks*: the relay answers the HTTP request
  with the `tool_use` block while the subprocess stays alive and blocked; the
  caller's next request resolves the parked call and the *same* subprocess
  resumes, preserving its context. A parked conversation holds a concurrency
  slot and is torn down after `RELAY_REQUEST_TIMEOUT` if the caller never
  returns a result. `tool_choice` is decoded but not enforced.

## [0.6.0] - 2026-07-11

### Added

- Per-request timeout: `X-Request-Timeout` (a Go duration) sets a single
  request's deadline; `RELAY_REQUEST_TIMEOUT` becomes both the default and
  the ceiling (longer requests are clamped, and the applied value is echoed
  back in the response header); a malformed value is a 400.
- Backpressure signals: 503 (pool busy) and the new 429 (quota exceeded)
  both carry a `Retry-After` header, and `RELAY_RATE_LIMIT_RPM` enables a
  per-caller token bucket (off by default; keyed by credential, or by remote
  address in the loopback no-token posture). Throttled requests spawn
  nothing and are counted in `/v1/metrics` as `rate_limited`.
- Session continuity: responses carry the backend conversation id
  (`X-Session-Id`), and sending it back resumes that conversation
  (`--resume`) instead of replaying a flattened transcript. Because the CLI
  keys sessions by working directory, resuming requires a stable workspace:
  inference mode, or an agentic request pinning a retained workspace with
  `X-Agentic-Outputs` — which together give a persistent agentic workspace
  (files *and* memory). Resuming without one is refused with an explanatory
  400; session ids are validated as UUIDs before reaching the CLI.
- Agent tool traces: the backend agent's own tool calls and results (parsed
  from the CLI's `assistant`/`user` stream-json lines, previously dropped)
  are surfaced two ways — opt-in SSE events on `/v1/messages`
  (`X-Agent-Traces: true` → `agent_tool_use` / `agent_tool_result`; off by
  default so strict SDK stream parsers are unaffected, and they consume no
  content-block indices), and a `trace.jsonl` written into retained output
  directories (`X-Agentic-Keep-Outputs`), created only if the agent actually
  used tools.
- Per-request cost and usage accounting: the backend-reported dollar cost
  (the claude CLI's `total_cost_usd`) and token counts are now surfaced —
  each served request logs a `request usage` line (`input_tokens`,
  `output_tokens`, `cost_usd`, correlated by `X-Request-Id`), and
  `/v1/metrics` gains `input_tokens_total`, `output_tokens_total`, and
  `cost_usd_total`. A client fanning requests out can attribute spend.
- `ROADMAP.md`: "Harness engineering" section — tool-activity traces,
  session continuity (`--resume`), backpressure signals, per-request timeout
  override.

## [0.5.0] - 2026-07-11

### Added

- Sampling parameters (`temperature`, `top_p`, `top_k`, `stop_sequences` /
  OpenAI `stop`) are decoded on both wires; backends declare whether they
  honor them (`Capabilities.Sampling`), and the relay logs a one-time
  warning naming the parameters it dropped instead of ignoring them
  silently.
- OpenAI streaming honors `stream_options: {"include_usage": true}`: a final
  chunk with empty `choices` carries token usage before `data: [DONE]`.

### Changed

- Usage now rides on `EventMessageStart` (input tokens) as well as
  `EventMessageStop`; the unused `EventUsage` event kind is removed. The
  Anthropic `message_start` event consequently reports real `input_tokens`
  instead of zero.

## [0.4.0] - 2026-07-11

### Added

- CI workflow (`.github/workflows/ci.yml`): gofmt, `go vet`, `go test -race`,
  and build, on Linux and macOS, for every push and pull request. No
  credentials needed — the suite drives a stub CLI and spends no tokens.
- `SECURITY.md`: security policy and threat model — assets, trust
  boundaries, what the relay does and does not defend against, why a
  TLS-terminating reverse proxy is mandatory off loopback, the security
  (not merely contractual) risks of sharing a caller token, ranked
  deployment postures, and private vulnerability reporting. Rendered on the
  documentation site and linked from the README, deployment, and
  execution-modes pages; `docs/deployment.md` gains Caddy and
  `tailscale serve` reverse-proxy examples.
- Agentic output retrieval: `X-Agentic-Keep-Outputs: true` on an
  agentic-authorized request retains its working directory under an
  unguessable id (`X-Agentic-Outputs` response header); new endpoints
  `GET /v1/outputs/{id}` (list), `GET /v1/outputs/{id}/files/{path}`
  (download), `DELETE /v1/outputs/{id}` (release); retained outputs are
  swept after `RELAY_OUTPUTS_TTL` (default 10m, `RELAY_OUTPUTS_DIR`
  configurable).

### Changed

- An expired request deadline now answers **504 Gateway Timeout** instead of
  502: the claude backend propagates the context cause (deadline or client
  disconnect) rather than the resulting "signal: killed", so a client can
  tell its own timeout from a backend failure.
- DQ-3 resolved: configuration is env-only, final (no file overlay); the
  lost requirements document is superseded by `spec.md` as the root
  authoritative source.

## [0.3.0] - 2026-07-11

### Added

- `docs/limitations.md`: honest map of what the Anthropic API offers that
  the CLI-backed relay cannot (tool calling, structured outputs, sampling
  control, caching…), with the bridged and degraded cases.
- Vision/PDF bridge: base64 `image` and `document` content blocks are
  accepted on `/v1/messages` (png/jpeg/gif/webp/pdf, 20 MiB decoded per
  block). The claude backend materializes attachments into a per-request
  ephemeral working directory and the CLI views them via its read-only Read
  tool; the directory is removed when the request ends.

## [0.2.0] - 2026-07-11

### Added

- Documentation site published with MkDocs Material on GitHub Pages
  (`mkdocs.yml` + `.github/workflows/docs.yml`); Go package documentation on
  pkg.go.dev.
- AGPL-3.0-or-later license.
- Terms-of-service disclaimer in the README and deployment docs (unofficial
  tool; personal-use scope for consumer subscriptions).
- Claude backend: ephemeral per-request working directory in agentic mode
  (REQ-EXEC-04) — created under `RELAY_CLAUDE_WORKDIR` (or the system temp
  dir) and removed when the request ends, isolating concurrent requests.
- Per-request agentic authorization (REQ-EXEC-06): with
  `RELAY_AGENTIC_PER_REQUEST_AUTHZ=true`, only requests presenting a valid
  `X-Agentic-Authorization` credential from `RELAY_AGENTIC_TOKENS` run
  agentically; others stay inference-only, invalid credentials get 403
  before any spawn. Backends independently refuse agentic requests they are
  not configured for.
- `docs/execution-modes.md`: in-depth explanation of inference vs agentic
  mode (enforcement, guarantees, caveats), summarized in the README.
- `ROADMAP.md`: known gaps and deferred features relative to the design
  document.
- Agentic audit trail: every request authorized to run agentically is logged
  at Info with its `X-Request-Id` and path; rejected agentic attempts are
  logged at Warn with a reason, alongside the `agentic_denied` metric.
- Honest `max_tokens`: backends declare whether they enforce it
  (`Capabilities.MaxTokens`; the claude CLI cannot), the relay logs a
  one-time warning when clients set it on a non-enforcing backend, and the
  OpenAI endpoint now also decodes `max_completion_tokens`.

### Changed

- Structured content support: the neutral model now carries content blocks
  (`text`, `tool_use`, `tool_result`) instead of plain strings. Both wire
  formats decode structured history, `tools[]`, and `tool_choice`; Anthropic
  responses can stream `tool_use` blocks (`input_json_delta`) and report
  `stop_reason: "tool_use"`. The claude backend flattens structured history
  into its text transcript; requests with client-defined `tools[]` are
  rejected with 400 on backends without client-tool support (the claude CLI
  has no raw tool-calling mode).

### Fixed

- Claude backend: when the CLI reports an error result line (e.g. "Credit
  balance is too low") and then exits non-zero, the parsed error message is
  now surfaced to the client instead of being masked by the bare
  "backend exited: exit status 1".

## [0.1.0] - 2026-07-11

### Added

- Neutral request/event model (`internal/core`): `InferRequest`, `Event`,
  `EventSink`, and the `Backend` interface with a self-registering backend
  factory registry.
- Claude backend adapter (`internal/backend/claude`): supervised `claude` CLI
  subprocess per request, prompt piped via stdin, defensive `stream-json`
  parsing, environment sanitization (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`,
  `CLAUDECODE`, plus operator deny list), and process-group kill on
  cancellation or timeout.
- Anthropic Messages API endpoint (`POST /v1/messages`) with SSE streaming
  and non-streaming responses.
- OpenAI Chat Completions API endpoint (`POST /v1/chat/completions`) with
  SSE streaming and non-streaming responses.
- Bearer / `x-api-key` authentication with constant-time token comparison;
  unauthenticated requests are rejected before any subprocess is spawned.
- Non-blocking concurrency limiter: a full pool answers 503 immediately.
- Per-request timeout enforcement via context.
- Env-first configuration with fail-fast startup guards: non-loopback binds
  refuse to start without auth tokens; agentic mode on a non-loopback bind
  refuses to start without per-request authorization.
- Agentic execution mode scaffold, disabled by default and loudly logged
  when enabled.
- Unauthenticated `GET /health`; authenticated `GET /v1/metrics` (minimal
  JSON counters).
- Request-ID middleware and structured JSON logging (`log/slog`).
- Test suite: wire-translation tables, limiter/dispatcher lifecycle with a
  fake backend, config-guard truth table, and Claude adapter tests driven by
  a stub CLI script (no tokens spent).
- Dockerfile (multi-stage, bundles the `claude` CLI) and docker-compose
  example; deployment documentation.

[Unreleased]: https://github.com/s-celles/agent-relay/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/s-celles/agent-relay/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/s-celles/agent-relay/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/s-celles/agent-relay/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/s-celles/agent-relay/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/s-celles/agent-relay/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/s-celles/agent-relay/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/s-celles/agent-relay/releases/tag/v0.1.0
