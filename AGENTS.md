# AGENTS.md — harness guide for coding agents

agent-relay is a Go HTTP relay that fronts the `claude` CLI (and an Ollama
backend) behind Anthropic-, OpenAI-, and A2A-compatible APIs. `spec.md` is the
authoritative design; `docs/` is the user-facing documentation.

## Feedback loop (use these, in this order)

| When | Command |
| --- | --- |
| While iterating | `just precommit` (gofmt + vet + tests, fast) |
| Single package | `go test ./internal/server/` |
| Single test | `go test ./internal/server/ -run TestName` |
| Coverage view | `just coverage` |
| Fuzz the untrusted-input surfaces | `just fuzz` (30s/target; seeds run in every `go test`) |
| Before ANY commit | `just check` (adds golangci-lint + race detector + govulncheck — same gates as CI) |

CI runs exactly the `just check` gate set plus an OpenAPI validation of
`docs/openapi.json`. If `just check` passes locally, CI should pass.

Tests never spend tokens: the suite spawns a stub `claude` script, never the
real CLI. Recipes under "release-time" in the justfile (`interop`,
`tools-check`, `smoke`) DO spend real tokens — never run them autonomously.

## Code map

| Package | Role |
| --- | --- |
| `cmd/relay` | Entry point: load config, wire server, serve. |
| `internal/config` | Env-first config; loaded once, validated, immutable. |
| `internal/core` | Neutral request/event model between API layer and backends. Depends on nothing else in the module; everything depends on it, never the reverse. |
| `internal/api/anthropic` | Anthropic Messages wire format ⇄ core. |
| `internal/api/openai` | OpenAI Chat Completions wire format ⇄ core. |
| `internal/api/a2a` | Agent2Agent surface (opt-in, wraps the A2A SDK — the only third-party dep). |
| `internal/backend/claude` | Adapts the `claude` CLI (spawn, flags, output parsing). Only package that knows the CLI. |
| `internal/backend/ollama` | Adapts a local Ollama server (HTTP client, no subprocess). |
| `internal/server` | HTTP mux: routing, auth, handlers. No framework. |
| `internal/toolbridge` | MCP bridge so the backend agent can call the caller's tools. |
| `internal/obs` | Request IDs, structured logging, JSON metrics. |
| `internal/outputs` | Retention of agentic-request artifacts (TTL, unguessable ids). |
| `internal/ratelimit` | Per-caller token bucket. |

Architecture rule: `core` stays dependency-free; wire adapters (`api/*`) and
backends (`backend/*`) plug into it. New code follows that direction.

## Conventions (enforced, not optional)

- TDD: write the failing test first, then the code.
- Conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`, …); SemVer.
- Every notable change gets a CHANGELOG.md entry (Keep a Changelog format).
- New features get a page or section under `docs/`; build docs warning-free
  (`mkdocs build --strict`) before committing doc changes.
- Never add AI co-author trailers or "Generated with …" lines anywhere.
- Upstream bugs found along the way go in `upstream-bugs.md`.
- Route registrations must be reflected in `docs/openapi.json` — a test
  enforces the correspondence.

## Hard limits

- NEVER read or print `.relay-token` / `.relay-agentic-token`.
- NEVER modify `.gitignore` (allowlist pattern; ask the user instead).
- NEVER `git add` CLAUDE.md, `specs/`, `.claude/`, `.specify/`.
- NEVER `git push --force`, never commit unless explicitly asked.
