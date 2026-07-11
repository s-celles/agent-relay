# Testing strategy

The suite is designed so that **no test ever spends a token** or requires a
logged-in `claude` CLI.

## Layers

- **Wire translation** (`internal/api/anthropic`, `internal/api/openai`) —
  table-driven tests in both directions: request decoding (string and block
  content, system prompts, role validation, malformed bodies) and event
  encoding (SSE streams and non-streaming JSON bodies).
- **Core lifecycle** (`internal/core`) — a `fakeBackend` implementing
  `core.Backend` drives the limiter, dispatcher, 503-on-full, slot release,
  cancellation, and timeout tests with no subprocess at all.
- **Claude adapter** (`internal/backend/claude`) — a **stub `claude` shell
  script** written to a temp dir emits canned `stream-json` lines. This
  exercises spawning, line parsing (including garbage and unknown-type lines),
  stdin prompt piping, environment sanitization, error results, spawn
  failures, and prompt kill-on-cancel.
- **Startup guards** (`internal/config`) — a truth table over
  `Config.Validate()` asserting every insecure bind/auth/agentic combination
  is rejected.
- **HTTP integration** (`internal/server`) — `httptest` requests through the
  full handler stack with a fake backend: auth (both header forms), streaming
  and non-streaming happy paths, 400/401/503 behavior, and metrics.

## Running

```sh
go test ./...
go test -race ./...   # required before committing
go vet ./...
gofmt -l .            # must print nothing
```

## Continuous integration

`.github/workflows/ci.yml` runs the same four gates (gofmt, vet, race tests,
build) on every push and pull request, on Linux and macOS. Because the suite
drives a stub `claude` script rather than the real CLI, CI needs no
credentials and spends no tokens.
