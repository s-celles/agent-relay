# A2A interoperability check

A manual check that drives the relay with the **official Python A2A SDK** —
an implementation nobody here wrote.

Script: [`docs/interop/a2a_interop.py`](interop/a2a_interop.py).

## Why it exists

The Go test suite was written by the same hand as the Go server, against the
same reading of the specification. That is a circularity: **if the reading is
wrong, the tests and the code agree with each other and pass anyway.** No
amount of Go tests can catch a misreading of the wire.

The A2A specification makes that risk concrete rather than theoretical. Its
prose contradicts its own normative protobuf in more than one place (the two we
hit are filed upstream as
[A2A#2044](https://github.com/a2aproject/A2A/issues/2044) and
[A2A#2045](https://github.com/a2aproject/A2A/issues/2045)), and the official
debugging tool, `a2a-inspector`, is still frozen at protocol 0.3 and rejects
conformant v1.0 agents outright
([a2a-inspector#145](https://github.com/a2aproject/a2a-inspector/pull/145)).
Reading the docs harder was never going to settle it.

So the check removes our reading from the loop. The Python SDK is handed **one
base URL and nothing else**: it fetches the Agent Card, decides from it which
transport to speak, and drives the relay from there — exactly as a third-party
peer would. Two implementations, written independently, in different languages,
agreeing on the wire is a fact about the wire, not about our opinion of it.

## What it checks

1. **Discovery** — the SDK's own resolver fetches and *validates* the Agent
   Card against the v1.0 model, and finds the JSON-RPC endpoint from it.
2. **A chat task** — `SendMessage` blocks to a terminal state and the answer
   comes back as an artifact.
3. **`GetTask`** — the task outlives the call that created it.
4. **Context continuity** — replaying the same `contextId` resumes the backend
   conversation: the agent recalls what it was told in the previous task.
5. **An agentic task** — the file the agent wrote comes back as a `url`
   artifact, and is fetchable out of band with the same bearer token (A2A
   defines no download endpoint of its own).

Step 5 is skipped when no agentic credential is supplied.

## Why it is not in CI

**Because it spends real money.** Every other test in this project is designed
so that [no test ever spends a token](testing.md) — the suite runs against a
stub `claude` script and needs no credentials at all. That property is worth
keeping: it is what lets CI run on every push, on a fork, from anyone.

This check is the opposite: it needs a running relay, a logged-in `claude` CLI,
a live subscription, and (for step 5) permission to write files on the host. It
is a **release-time check you run deliberately**, not a gate on every commit.
Putting it in CI would either break the no-cost invariant or require secrets a
fork cannot have.

It also needs Python, which nothing else here does.

## Running it

Start a relay with A2A enabled:

```sh
RELAY_BIND=127.0.0.1:18082 \
RELAY_TOKENS=t0ken \
RELAY_A2A_ENABLED=true \
RELAY_A2A_MODEL=haiku \
RELAY_PUBLIC_URL=http://127.0.0.1:18082 \
RELAY_AGENTIC_ENABLED=true \
RELAY_AGENTIC_PER_REQUEST_AUTHZ=true \
RELAY_AGENTIC_TOKENS=agent-secret \
RELAY_AGENTIC_ARGS='--permission-mode,acceptEdits' \
  ./relay
```

Then, in a throwaway virtualenv:

```sh
python3 -m venv .venv
.venv/bin/pip install 'a2a-sdk>=1.0' httpx

RELAY_URL=http://127.0.0.1:18082 \
RELAY_TOKEN=t0ken \
RELAY_AGENTIC_TOKEN=agent-secret \
  .venv/bin/python docs/interop/a2a_interop.py
```

Expected output:

```
[1] Discovery — the SDK reads the Agent Card
  [ok] the card parses against the v1.0 model — agent-relay
  [ok] a JSON-RPC interface is advertised — JSONRPC 1.0 @ http://127.0.0.1:18082/a2a
  [ok] streaming is advertised
  [ok] skills are advertised — chat, agentic-task

[2] SendMessage — a chat task
  [ok] the task reaches a terminal state — TASK_STATE_COMPLETED
  [ok] the answer comes back as an artifact — 'INTEROP OK'
  [ok] the server generated both ids

[3] GetTask
  [ok] the task can be fetched back — TASK_STATE_COMPLETED

[4] Same contextId — does the agent remember?
  [ok] the context is preserved
  [ok] the backend session resumed — '"INTEROP OK"'

[5] Agentic task — files returned as url artifacts
  [ok] the agentic task completed — TASK_STATE_COMPLETED
  [ok] the file it wrote is an artifact — interop.txt, trace.jsonl
  [ok] the artifact is fetchable out of band — HTTP 200: 'OK'

All interop checks passed.
```

Exit status: `0` all passed, `1` a check failed, `2` the probe could not run
(relay unreachable, A2A disabled, bad token).

## When to run it

- Before tagging a release, if anything under `internal/api/a2a` changed.
- After bumping the `a2a-go` dependency.
- When a peer reports that it cannot talk to your relay — this tells you within
  a minute whether the fault is on your side of the wire.
