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
just precommit   # fast inner loop: gofmt + vet + tests
just check       # full pre-commit gate: adds lint, race detector, govulncheck
just coverage    # per-package statement coverage
```

Or the underlying commands directly:

```sh
go test ./...
go test -race ./...   # required before committing
go vet ./...
gofmt -l .            # must print nothing
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...
```

Static analysis beyond `go vet` (staticcheck, errcheck, unused, …) is
configured in `.golangci.yml`; `errcheck` is relaxed in test files, where an
unchecked error is deliberate shorthand.

## Fuzzing

The three surfaces that parse untrusted input have native Go fuzz targets:

- `internal/api/anthropic` and `internal/api/openai` — `FuzzDecodeRequest`,
  the request-body decoders behind `/v1/messages` and
  `/v1/chat/completions`. The property checked: never panic, and any
  *accepted* request satisfies the invariants the rest of the relay relies
  on (non-empty model, at least one message, valid roles).
- `internal/toolbridge` — `FuzzHandleMCP`, the JSON-RPC endpoint the CLI
  subprocess speaks to. The property: never panic, always answer with an
  expected status; a resolver goroutine drains parked tool calls so
  `tools/call` inputs cannot wedge the loop.

The seed corpora run on every ordinary `go test` (so CI exercises them);
`just fuzz` (default 30s per target, `just fuzz 5m` for longer) explores
beyond the seeds.

## Continuous integration

`.github/workflows/ci.yml` runs the same gates (gofmt, vet, golangci-lint,
race tests, build, govulncheck) on every push and pull request, on Linux and
macOS, plus a validation of `docs/openapi.json` as a real OpenAPI document. Because the suite drives a stub
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
