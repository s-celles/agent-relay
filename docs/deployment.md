# Deployment

> **Terms of service**: relayed requests run under your Anthropic account's
> terms. Consumer subscriptions are personal — deploy the relay for your own
> use on a private network only, and never share or resell access to your
> account through it. See the [disclaimer](index.md#disclaimer-terms-of-service).

The runtime image is **not** just the Go binary: the Claude backend spawns
the `claude` CLI, so the image bundles the CLI (and its Node runtime), and
the host's subscription auth is provided via a mounted volume.

## Docker

```sh
docker build -t agent-relay .
```

The provided `Dockerfile` is a two-stage build: a Go builder producing a
static `relay` binary, and a `node`-based runtime with
`@anthropic-ai/claude-code` installed globally.

First-time (headless) login, once per volume:

```sh
docker compose up -d
docker compose exec relay claude login
```

The `/home/relay/.claude` volume persists the login across restarts.

## docker-compose

See `docker-compose.yml`. Adjust `RELAY_BIND` to your private interface
(e.g. the Tailscale IP) and set a strong token:

```sh
RELAY_TOKENS=$(openssl rand -hex 32) docker compose up -d
```

Remember the startup guard: a non-loopback bind without `RELAY_TOKENS`
refuses to start.

## NixOS

On NixOS hosts, build with `buildGoModule` and expose the relay as a systemd
service bound to `tailscale0` — the fully declarative path:

```nix
{ buildGoModule }:
buildGoModule {
  pname = "agent-relay";
  version = "0.1.0";
  src = ./.;
  vendorHash = null; # stdlib only, no dependencies
}
```

A minimal service unit sets the `RELAY_*` environment and runs the binary as
an unprivileged user with the `claude` CLI on `PATH`.

## Operational notes

- `GET /health` is unauthenticated and suitable for container health checks.
- One subprocess per in-flight request: size `RELAY_MAX_CONCURRENT` to the
  host's memory/CPU budget; excess requests receive an immediate 503.
- Structured JSON logs go to stderr; each request logs its `X-Request-Id`.
