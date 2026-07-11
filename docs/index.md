# agent-relay

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

!!! warning "Read the threat model before exposing the relay"

    The relay speaks plain HTTP, runs the CLI as your OS user, and can be
    granted the right to write files and run commands. Off loopback, a
    TLS-terminating reverse proxy is **mandatory**, and a shared caller token
    means a shared identity. See
    [Security & threat model](security.md).

## Reading guide

- [Security & threat model](security.md) — risks, trust boundaries, deployment postures
- [Architecture](architecture.md) — the three-layer pipeline and neutral model
- [Backends & routing](backends.md) — claude and ollama, routing by model name
- [Execution modes](execution-modes.md) — inference vs agentic, in depth
- [Configuration](configuration.md) — environment variables and startup guards
- [HTTP API](api.md) — endpoints, wire formats, error shapes
- [API vs relay limitations](limitations.md) — what the CLI backend cannot do
- [Deployment](deployment.md) — Docker, docker-compose, NixOS notes
- [Testing](testing.md) — test strategy (no tokens are ever spent in tests)

## Disclaimer — terms of service

This is an independent, self-hosted tool, **not affiliated with, endorsed
by, or supported by Anthropic** or any other model provider. Relayed
requests run under your own account's terms; consumer subscriptions are
personal — deploy the relay for your own use on a private network only, and
never share or resell access to your account through it. See the full
[disclaimer in the repository README](https://github.com/s-celles/agent-relay#disclaimer--terms-of-service).

The project is licensed under the
[AGPL-3.0-or-later](https://github.com/s-celles/agent-relay/blob/main/LICENSE);
Go package documentation lives on
[pkg.go.dev](https://pkg.go.dev/github.com/s-celles/agent-relay).
