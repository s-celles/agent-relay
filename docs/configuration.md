# Configuration

Configuration is environment-first (12-factor, container-friendly). It is
loaded once at startup, validated, then immutable. Any parse or validation
failure aborts startup — the relay never limps along on a half-read config.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `RELAY_BIND` | `127.0.0.1:18082` | Listen address. Defaults to loopback. |
| `RELAY_TOKENS` | *(empty)* | Comma-separated caller bearer tokens. Mandatory on non-loopback binds. |
| `RELAY_BACKEND` | `claude` | Backend to serve (v1 ships `claude` only). |
| `RELAY_MAX_CONCURRENT` | `10` | Max simultaneous backend subprocesses; excess requests get 503. |
| `RELAY_REQUEST_TIMEOUT` | `10m` | Per-request timeout (Go duration syntax, e.g. `90s`). |
| `RELAY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `RELAY_CLAUDE_CLI` | `claude` | Path to the `claude` binary. |
| `RELAY_CLAUDE_WORKDIR` | *(empty)* | Working directory for the subprocess (empty = inherit). |
| `RELAY_CLAUDE_MODEL_MAP` | *(empty)* | Logical→backend model table, e.g. `sonnet=claude-sonnet-5,haiku=claude-haiku-4-5`. Unmapped names pass through unchanged. |
| `RELAY_ENV_DENY` | *(empty)* | Extra env keys stripped from the subprocess, comma-separated. |
| `RELAY_AGENTIC_ENABLED` | `false` | Opt in to host-side agentic execution. Loudly logged at startup. |
| `RELAY_AGENTIC_PER_REQUEST_AUTHZ` | `false` | Required to combine agentic mode with a non-loopback bind. |
| `RELAY_AGENTIC_ARGS` | *(empty)* | Permission flags appended to the CLI when agentic mode is on. |

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
