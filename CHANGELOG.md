# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/s-celles/agent-relay/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/s-celles/agent-relay/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/s-celles/agent-relay/releases/tag/v0.1.0
