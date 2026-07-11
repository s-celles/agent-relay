# Configuration

Configuration is environment-first (12-factor, container-friendly). It is
loaded once at startup, validated, then immutable. Any parse or validation
failure aborts startup — the relay never limps along on a half-read config.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `RELAY_BIND` | `127.0.0.1:18082` | Listen address. Defaults to loopback. |
| `RELAY_TOKENS` | *(empty)* | Comma-separated caller bearer tokens. Mandatory on non-loopback binds. |
| `RELAY_BACKEND` | `claude` | Default backend: serves every model without a route. |
| `RELAY_MODEL_ROUTES` | *(empty)* | Model→backend routing, e.g. `llama3=ollama,phi3=ollama`. The client keeps choosing a `model`; the relay decides which backend that means. A route to an unknown backend refuses to start. |
| `RELAY_OLLAMA_URL` | `http://127.0.0.1:11434` | Ollama server for the `ollama` backend. |
| `RELAY_OLLAMA_MODEL_MAP` | *(empty)* | Logical→Ollama model table, same syntax as the claude one. |
| `RELAY_MAX_CONCURRENT` | `10` | Max simultaneous backend subprocesses; excess requests get 503. |
| `RELAY_REQUEST_TIMEOUT` | `10m` | Default **and ceiling** for a request's deadline (Go duration, e.g. `90s`). Clients may ask for less with the `X-Request-Timeout` header; more is clamped to this value. |
| `RELAY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `RELAY_CLAUDE_CLI` | `claude` | Path to the `claude` binary. |
| `RELAY_CLAUDE_WORKDIR` | *(empty)* | Working directory for the subprocess (empty = inherit). In agentic mode this is the *parent* under which each request gets its own ephemeral directory. |
| `RELAY_CLAUDE_MODEL_MAP` | *(empty)* | Logical→backend model table, e.g. `sonnet=claude-sonnet-5,haiku=claude-haiku-4-5`. Unmapped names pass through unchanged. |
| `RELAY_ENV_DENY` | *(empty)* | Extra env keys stripped from the subprocess, comma-separated. |
| `RELAY_AGENTIC_ENABLED` | `false` | Opt in to host-side agentic execution. Loudly logged at startup. Each agentic request runs in its own ephemeral working directory, deleted when the request ends. |
| `RELAY_AGENTIC_PER_REQUEST_AUTHZ` | `false` | Require a per-request agentic credential: only requests carrying a valid `X-Agentic-Authorization` header run agentically, all others stay inference-only. Required to combine agentic mode with a non-loopback bind. |
| `RELAY_AGENTIC_TOKENS` | *(empty)* | Comma-separated agentic credentials, mandatory when per-request authz is on. Keep them distinct from `RELAY_TOKENS`. |
| `RELAY_AGENTIC_ARGS` | *(empty)* | Permission flags appended to the CLI when agentic mode is on. |
| `RELAY_OUTPUTS_DIR` | *(system temp)* | Where retained agentic outputs (`X-Agentic-Keep-Outputs`) are stored. |
| `RELAY_OUTPUTS_TTL` | `10m` | How long retained outputs survive before being swept. |
| `RELAY_RATE_LIMIT_RPM` | `0` (off) | Sustained requests per minute allowed **per caller** (token bucket, burst = the same value). Exceeding it returns 429 with `Retry-After`. |
| `RELAY_A2A_ENABLED` | `false` | Serve the [Agent2Agent](a2a.md) adapter: `POST /a2a` (JSON-RPC, authenticated) and a **public** Agent Card at `/.well-known/agent-card.json`. Off by default: the card is unauthenticated and names the models served. |
| `RELAY_A2A_MODEL` | `sonnet` | Model for A2A messages that name none — A2A has no model field (peers may still set `message.metadata.model`). |
| `RELAY_PUBLIC_URL` | `http://$RELAY_BIND` | The origin **peers** reach this relay on: what the Agent Card advertises and what artifact URLs are built from. Required when A2A is on and the bind address is not loopback. |

The `RELAY_AGENTIC_*` variables switch the relay between its two execution
modes; [execution-modes.md](execution-modes.md) explains what each mode can
and cannot do.

## Startup guards

`Config.Validate()` encodes the project's anti-goals as invariants. The relay
**refuses to start** when:

- the bind address is non-loopback and no tokens are configured;
- agentic mode is enabled on a non-loopback bind without per-request
  authorization;
- no backend is configured, or `RELAY_MAX_CONCURRENT < 1`.

There is, by construction, no configuration in which an unauthenticated
caller on a non-loopback interface reaches a backend.

## Environment sanitization

The subprocess never sees `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, or
`CLAUDECODE`, regardless of configuration. Add operator-specific secrets to
`RELAY_ENV_DENY` to strip them as well.
