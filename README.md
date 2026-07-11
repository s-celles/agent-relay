# agent-relay

[![ci](https://github.com/s-celles/agent-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/s-celles/agent-relay/actions/workflows/ci.yml)
[![docs](https://github.com/s-celles/agent-relay/actions/workflows/docs.yml/badge.svg)](https://s-celles.github.io/agent-relay/)
[![Go Reference](https://pkg.go.dev/badge/github.com/s-celles/agent-relay.svg)](https://pkg.go.dev/github.com/s-celles/agent-relay)
[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)

A self-hosted, authenticating **inference relay** that fronts agent CLIs
(v1: the `claude` CLI) behind standard HTTP APIs:

- `POST /v1/messages` — Anthropic Messages API (streaming SSE and non-streaming)
- `POST /v1/chat/completions` — OpenAI Chat Completions API
- `GET /health` — unauthenticated liveness probe
- `GET /v1/metrics` — minimal JSON metrics

The relay spawns one supervised CLI subprocess per request, translates its
`stream-json` output into the requested wire format, and enforces a hard
security invariant: **there is no configuration in which an unauthenticated
caller on a non-loopback interface reaches a backend.**

## Two execution modes

| | Inference (default) | Agentic (opt-in) |
|---|---|---|
| What a request can do | produce text, spend tokens | also create/edit files, run granted commands |
| CLI permission flags | none, ever | operator-chosen, per authorized request |
| Working directory | static | ephemeral per request, auto-deleted |
| Extra credential | — | `X-Agentic-Authorization` header |

In **inference mode** the CLI's tools hit a permission wall that nothing in a
non-interactive subprocess can lift — callers get text, never side effects.
**Agentic mode** deliberately lifts that wall behind four layers of consent:
an operator flag, startup guards, an optional per-request credential, and a
backend re-check. Each agentic request runs isolated in its own throwaway
directory. The full reasoning, guarantees, and caveats are in
[docs/execution-modes.md](docs/execution-modes.md).

## Quick start

```sh
go build -o relay ./cmd/relay

# loopback, no auth required
./relay

# on a private network interface (e.g. Tailscale), auth is mandatory
RELAY_BIND=100.64.0.5:18082 RELAY_TOKENS=$(openssl rand -hex 32) ./relay
```

Call it with any Anthropic- or OpenAI-compatible client:

```sh
curl -N http://127.0.0.1:18082/v1/messages \
  -H "x-api-key: <token>" \
  -d '{"model":"sonnet","max_tokens":1024,"stream":true,
       "messages":[{"role":"user","content":"hello"}]}'
```

## Security

**Read [SECURITY.md](SECURITY.md) before exposing the relay off loopback.**
It states the threat model, the trust boundaries, what the relay does *not*
defend against (TLS, sandboxing, prompt injection, per-caller quotas), why a
TLS-terminating reverse proxy is mandatory off loopback, and why sharing a
caller token means sharing your identity, your subscription's reputation, and
— in agentic mode — your host.

## Documentation

- [Security & threat model](SECURITY.md) — risks, boundaries, deployment postures
- [Architecture](docs/architecture.md) — the three-layer pipeline and neutral model
- [Execution modes](docs/execution-modes.md) — inference vs agentic, in depth
- [Configuration](docs/configuration.md) — environment variables and startup guards
- [API](docs/api.md) — endpoints, wire formats, error shapes
- [Deployment](docs/deployment.md) — Docker, docker-compose, NixOS notes
- [Testing](docs/testing.md) — test strategy (no tokens are ever spent in tests)

## Development

```sh
go test ./...        # full suite, subprocess tests use a stub CLI
go test -race ./...  # run before committing
go vet ./...
```

Versioning follows [Semantic Versioning](https://semver.org/); notable changes
are tracked in [CHANGELOG.md](CHANGELOG.md).

## Disclaimer — terms of service

This is an independent, self-hosted tool. It is **not affiliated with,
endorsed by, or supported by Anthropic** or any other model provider.

- Requests relayed to the `claude` CLI are subject to the terms that govern
  your Anthropic account — the Consumer or Commercial Terms of Service and
  the Usage Policy — exactly as if you had run the CLI yourself.
- Consumer subscriptions (Pro/Max) are **personal**. This relay is designed
  for your own scripts and devices on a private network (e.g. a Tailnet).
  Do not expose it to third parties, share access to your account through
  it, or use it to resell access. If several people or a service need
  access, use an API key under commercial terms instead.
- Providers may restrict automated or programmatic use of consumer
  subscriptions; review the current terms before deploying. Violations can
  lead to rate limiting, suspension, or termination of your account.
- You are solely responsible for how you deploy and use this software. It is
  provided under the AGPL **without any warranty** (see [LICENSE](LICENSE)).

## AI usage disclosure

Portions of this project (code, tests, and documentation) were developed with
the assistance of AI tools, under human direction and review.

## License

This project is licensed under the GNU Affero General Public License,
version 3 or (at your option) any later version — see [LICENSE](LICENSE).
