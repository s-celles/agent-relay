# Deployment

> **Before deploying, read the [security & threat model](security.md).** Off
> loopback the relay requires a TLS-terminating reverse proxy; agentic mode
> with broad permissions must be containerized; and a shared caller token is
> a shared identity on your subscription and host.
>
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

## Reverse proxy (mandatory off loopback)

The relay speaks plain HTTP and has no per-caller rate limit by design. Off
loopback, front it with a proxy that terminates TLS and throttles abuse — see
the [security & threat model](security.md#why-a-reverse-proxy-is-not-optional-off-loopback).

With Caddy (automatic TLS, rate limiting via the `caddy-ratelimit` plugin):

```caddyfile
relay.example.ts.net {
    encode gzip
    rate_limit {
        zone relay {
            key    {http.request.header.x-api-key}
            events 60
            window 1m
        }
    }
    reverse_proxy 127.0.0.1:18082 {
        flush_interval -1   # required: do not buffer SSE streams
    }
}
```

With Tailscale alone (no extra TLS setup — `tailscale serve` terminates TLS
with a Tailnet certificate):

```sh
tailscale serve --bg --https=443 http://127.0.0.1:18082
```

Whichever you choose: keep the relay bound to `127.0.0.1` and let the proxy
be the only thing listening on the network interface, and make sure streaming
responses are **not** buffered (`flush_interval -1` in Caddy,
`proxy_buffering off` in nginx) or SSE will arrive in one chunk at the end.

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
