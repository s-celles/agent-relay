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
build) on every push and pull request, on Linux and macOS, plus a validation of
`docs/openapi.json` as a real OpenAPI document. Because the suite drives a stub
`claude` script rather than the real CLI, CI needs no credentials and spends no
tokens.

## The one check that is *not* automated

Everything above is closed-loop: the Go tests were written against the same
reading of the specifications as the Go code, so they cannot catch a
misreading of the wire — they would simply agree with it.

The [A2A interoperability check](interop.md) breaks that circularity by driving
the relay with the official **Python** A2A SDK, which knows nothing about this
project. It is deliberately kept out of CI: it needs a live subscription and
spends real tokens, and the no-cost invariant above is worth more than the
convenience of automating it. Run it by hand before tagging a release that
touched `internal/api/a2a`.
