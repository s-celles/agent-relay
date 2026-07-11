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

## Documentation

- [Architecture](docs/architecture.md) — the three-layer pipeline and neutral model
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
