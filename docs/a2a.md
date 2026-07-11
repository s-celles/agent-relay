# Agent2Agent (A2A)

The relay can present itself as an **A2A agent**: peers discover it through an
Agent Card, send it *tasks*, stream the work as it happens, and collect the
files it produced as *artifacts*.

This is a third wire adapter, next to the Anthropic and OpenAI ones. It is not
a backend and not a router: **the relay does not call other agents — it is
one.** Nothing about model routing, the tool bridge, or agentic execution
changes; A2A is another way in.

!!! warning "Opt-in, and it publishes a public card"

    A2A is off unless `RELAY_A2A_ENABLED=true`. When on, the Agent Card is
    served **without authentication** — discovery is the point of a card, and a
    peer must read it before it holds any credential. The card names the models
    you serve and states whether this host can run an agent. Turn A2A on
    deliberately, and read [Security](security.md).

## Why it is interesting here

The Anthropic and OpenAI wires model a *conversation*: you send messages, you
get text back. They have no vocabulary for the thing the relay is actually good
at — **a long-running task that produces files**.

A2A does. It has tasks with a lifecycle, cancellation, and artifacts. That maps
onto agentic execution almost exactly:

| A2A | agent-relay |
|---|---|
| Task | one agentic (or inference) request |
| `contextId` | the backend session **and** its retained workspace |
| Artifact (`url` part) | a file from the retained workspace, served by `/v1/outputs/…` |
| `statusUpdate` / `artifactUpdate` | the backend's event stream |
| `CancelTask` | process-group kill of the CLI subprocess |
| Bearer scheme on the card | `RELAY_TOKENS`, as everywhere else |

## Enabling it

```sh
RELAY_A2A_ENABLED=true \
RELAY_A2A_MODEL=sonnet \
RELAY_PUBLIC_URL=https://relay.example.com \
RELAY_TOKENS=$(openssl rand -hex 32) \
./relay
```

`RELAY_PUBLIC_URL` matters: it is the origin the card advertises and the origin
artifact URLs are built from, so it must be the address a **peer** can resolve —
not the bind address, when a reverse proxy or Tailscale sits in front. A relay
bound off-loopback that still advertises `127.0.0.1` refuses to start.

Two endpoints appear:

```
GET  /.well-known/agent-card.json   # public, no auth
POST /a2a                           # JSON-RPC 2.0, bearer auth + rate limit
```

Methods: `SendMessage`, `SendStreamingMessage`, `GetTask`, `CancelTask`,
`ListTasks`. The JSON-RPC binding is the only one served — gRPC and HTTP+JSON
are optional in the spec.

## Sending a task

A2A is an *agent* protocol, so a message carries no model field. Name one in
`message.metadata.model` when you care; otherwise `RELAY_A2A_MODEL` serves.

```sh
curl -s -X POST https://relay.example.com/a2a \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":
       {"message":{"messageId":"m1","role":"ROLE_USER",
                   "parts":[{"text":"Summarize this changelog in three bullets."}],
                   "metadata":{"model":"haiku"}}}}'
```

`SendMessage` **blocks until the task reaches a terminal state** (that is the
v1.0 default) and answers with the whole task; the model's reply is an artifact
named `response`:

```json
{"jsonrpc":"2.0","id":1,"result":{"task":{
  "id":"019f521e-…", "contextId":"019f521e-…",
  "status":{"state":"TASK_STATE_COMPLETED"},
  "artifacts":[{"artifactId":"…","name":"response","parts":[{"text":"…"}]}]
}}}
```

Use `SendStreamingMessage` with `Accept: text/event-stream` to watch it happen:
an initial `task`, then `statusUpdate` (`TASK_STATE_WORKING`), then
`artifactUpdate` chunks as the text arrives, then a terminal `statusUpdate`.

A backend failure is a **`TASK_STATE_FAILED` task, not a JSON-RPC error** — the
reason is in `status.message`. The error channel is reserved for protocol and
authorization faults.

### Attachments

Images and PDFs ride as `raw` parts (base64, with a `mediaType`), and go through
the same [attachment bridge](api.md#attachments-images-and-pdfs) as the other
wires.

`url` parts are **refused**. Fetching a URL a peer chose would make the relay an
SSRF primitive against whatever it can reach; send the bytes instead.

## Agentic tasks and artifacts

This is where A2A earns its place. Present the agentic credential on the A2A
call — the same header as everywhere else — and the task runs agentically:

```sh
curl -s -X POST https://relay.example.com/a2a \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Agentic-Authorization: Bearer $AGENTIC_TOKEN" \
  -d '{"jsonrpc":"2.0","id":2,"method":"SendMessage","params":
       {"message":{"messageId":"m2","role":"ROLE_USER","parts":[{"text":
         "Write primes.txt with the first 5 primes, then reply DONE."}]}}}'
```

The task gets a **retained workspace**, and every file the agent left there
comes back as an artifact — a `url` part, since A2A defines no download
endpoint:

```json
"artifacts":[
  {"artifactId":"…","name":"response","parts":[{"text":"DONE"}]},
  {"artifactId":"primes.txt","name":"primes.txt","parts":[
    {"url":"https://relay.example.com/v1/outputs/f03f…/files/primes.txt",
     "filename":"primes.txt","mediaType":"text/plain","metadata":{"size":12}}]}
]
```

Fetch it out of band, with the same bearer token:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  https://relay.example.com/v1/outputs/f03f…/files/primes.txt
```

Artifacts expire with the workspace (`RELAY_OUTPUTS_TTL`).

### Continuing in the same context

Echo the `contextId` on the next message and the task lands **in the same
conversation and the same workspace**: the backend session resumes (it
remembers), and the files are still there (it can read and extend them).

```sh
  -d '{… "params":{"message":{"messageId":"m3","contextId":"019f521e-…",
        "role":"ROLE_USER","parts":[{"text":"Append the next prime to that file."}]}}}'
```

That combination — memory *and* a filesystem that persists across calls, driven
by a standard protocol — is what neither the Anthropic nor the OpenAI wire can
express.

## Cancellation

`CancelTask` cancels the dispatch context, which for the `claude` backend means
a process-group kill: the subprocess is gone, not merely detached from the
stream.

```sh
curl -s -X POST … -d '{"jsonrpc":"2.0","id":3,"method":"CancelTask","params":{"id":"019f…"}}'
```

## What is not implemented

- **Push notifications** (`pushNotifications` is `false` on the card) — the
  relay does not call out to a peer's webhook.
- **gRPC and HTTP+JSON bindings** — optional in the spec; JSON-RPC only.
- **Client-defined tools over A2A.** They work on the Anthropic and OpenAI wires
  through the [MCP tool bridge](api.md#client-defined-tools); exposing them over
  A2A needs a declared protocol extension, which is not written.
- **Multi-tenancy** (`tenant`) and the extended agent card.

## The one dependency

The protocol machinery — JSON-RPC binding, SSE, task store, agent card, the task
state machine — is the official
[`a2a-go`](https://github.com/a2aproject/a2a-go) SDK's, and it is the only
non-standard-library dependency in the project (JSON-RPC only: no gRPC, no
protobuf).

That is a deliberate exception to the stdlib-only rule. A2A v1.0 was a breaking
redesign of 0.3 and is still moving; hand-rolling the wire would mean our tests
validated *our reading* of the specification rather than the specification —
and the specification's own prose contradicts its normative proto in at least
two places (see `upstream-bugs.md`). The core, the backends and the security
path remain standard-library only.
