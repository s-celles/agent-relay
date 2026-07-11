# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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

[Unreleased]: https://github.com/s-celles/agent-relay/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/s-celles/agent-relay/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/s-celles/agent-relay/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/s-celles/agent-relay/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/s-celles/agent-relay/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/s-celles/agent-relay/releases/tag/v0.1.0
