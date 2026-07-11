# agent-relay — developer tasks.  Run `just` to list them.
#
# The caller token is a strong random value, generated once and persisted to a
# gitignored file, so `just run` (one terminal) and `just opencode` (another)
# read the SAME token — a per-invocation random would differ between them and
# 401. Put your own value in the file to override, or `just new-token` to roll.
#
# (The relay denies ANTHROPIC_API_KEY to the CLI by default, so the
# subscription is used and a stray key in your shell cannot hijack it.)

token_file := ".relay-token"
agentic_token_file := ".relay-agentic-token"
bind := "127.0.0.1:18082"
url := "http://" + bind

# List recipes (default).
default:
    @just --list

# Create the token files if absent (strong random, 0600, gitignored).
_tokens:
    @test -f {{token_file}} || {{ 'sh -c "umask 077; openssl rand -hex 32 > ' + token_file + ' && echo generated ' + token_file + '"' }}
    @test -f {{agentic_token_file}} || {{ 'sh -c "umask 077; openssl rand -hex 32 > ' + agentic_token_file + ' && echo generated ' + agentic_token_file + '"' }}

# Regenerate the caller token (invalidates the current one).
new-token:
    @sh -c "umask 077; openssl rand -hex 32 > {{token_file}}" && echo "rolled {{token_file}}"

# Print the caller token (e.g. to paste into another tool).
print-token: _tokens
    @cat {{token_file}}

# Build the relay binary into the repo root.
build:
    go build -o relay ./cmd/relay

# Remove build artifacts (the relay binary and the mkdocs site directory).
clean:
    rm -rf relay site

# Run the relay — plain inference, auth on, subscription-backed.
run: build _tokens
    RELAY_TOKENS="$(cat {{token_file}})" ./relay

# Run with model-name routing to a local Ollama, alongside the claude default.
# Unrouted names (haiku, sonnet, opus…) still go to your subscription.
#
# Only tool-capable models are routed: an agent client sends its toolset on
# every request, so a model without tool calling fails even a plain "hello".
# And the `tools` badge is necessary but not sufficient — qwen2.5-coder carries
# it yet writes the call as *text* instead of emitting a structured tool_call,
# so the client never runs it. These three were measured to emit real calls:
#
#   granite4.1:3b  ~3s   ← fastest; the one to use
#   llama3.1:8b    ~10s
#   qwen3.5        ~13s+ (thinking; slowest)
run-hybrid: build _tokens
    RELAY_TOKENS="$(cat {{token_file}})" \
    RELAY_MODEL_ROUTES="granite4.1:3b=ollama,llama3.1:8b=ollama,qwen3.5=ollama" \
    ./relay

# Run with agentic execution enabled (file edits only: acceptEdits).
# Grants more than inference — read docs/execution-modes.md before using.
run-agentic: build _tokens
    RELAY_TOKENS="$(cat {{token_file}})" \
    RELAY_AGENTIC_ENABLED=true \
    RELAY_AGENTIC_PER_REQUEST_AUTHZ=true \
    RELAY_AGENTIC_TOKENS="$(cat {{agentic_token_file}})" \
    RELAY_AGENTIC_ARGS=--permission-mode,acceptEdits \
    ./relay

# Run with the Agent2Agent surface enabled (publishes a public Agent Card).
run-a2a: build _tokens
    RELAY_TOKENS="$(cat {{token_file}})" \
    RELAY_A2A_ENABLED=true \
    RELAY_A2A_MODEL=haiku \
    RELAY_PUBLIC_URL={{url}} \
    ./relay

# Liveness check against a running relay.
health:
    curl -fsS {{url}}/health && echo

# One inference round-trip, to prove the subscription path works end to end.
smoke: _tokens
    curl -fsS {{url}}/v1/messages -H "x-api-key: $(cat {{token_file}})" \
      -d '{"model":"haiku","max_tokens":50,"messages":[{"role":"user","content":"reply with exactly: OK"}]}' \
      && echo

# Launch OpenCode against the relay, with the matching token.
# The global ~/.config/opencode/opencode.jsonc already points at this relay.
opencode: _tokens
    RELAY_TOKEN="$(cat {{token_file}})" opencode

# --- quality gates (what CI runs) ----------------------------------------

# Format, vet, lint, race-test, and vuln-scan — the full CI gate set.
check: fmt-check vet lint test-race vuln

# Fast inner-loop gate while iterating: format, vet, plain tests.
# Run `just check` before committing — it adds lint, the race detector and
# the vuln scan.
precommit: fmt vet test

fmt:
    gofmt -w .

fmt-check:
    @unformatted=$(gofmt -l .); \
    if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi

vet:
    go vet ./...

test:
    go test ./...

test-race:
    go test -race ./...

# Static analysis beyond vet (staticcheck, errcheck, …) — see .golangci.yml.
lint:
    go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...

# Per-package statement coverage, so gaps stay visible in the feedback loop.
coverage:
    go test -cover ./...

# Fuzz the wire decoders and the MCP endpoint (the untrusted-input surfaces).
# Seeds always run as part of `go test`; this digs deeper. Adjust with
# `just fuzz 5m` for a longer session.
fuzz fuzztime="30s":
    go test -fuzz=FuzzDecodeRequest -fuzztime={{fuzztime}} ./internal/api/anthropic/
    go test -fuzz=FuzzDecodeRequest -fuzztime={{fuzztime}} ./internal/api/openai/
    go test -fuzz=FuzzHandleMCP -fuzztime={{fuzztime}} ./internal/toolbridge/

# Fail on any reachable known vulnerability (stdlib or the one dependency).
vuln:
    go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# --- release-time, spends real tokens (never in CI) ----------------------

# A2A interoperability check: drive the relay with the official Python SDK.
# Needs a relay running with `just run-a2a` and the python deps installed
# (see docs/interop.md). Spends real tokens.
interop: _tokens
    RELAY_URL={{url}} RELAY_TOKEN="$(cat {{token_file}})" \
    RELAY_AGENTIC_TOKEN="$(cat {{agentic_token_file}})" \
      python docs/interop/a2a_interop.py

# Client-tool check: drive the MCP tool bridge with the official Anthropic SDK,
# across several tool_use patterns. Needs a relay running (`just run`) and
# `pip install anthropic`. Spends real tokens.
tools-check: _tokens
    RELAY_URL={{url}} RELAY_TOKEN="$(cat {{token_file}})" \
      python docs/interop/tools_check.py
