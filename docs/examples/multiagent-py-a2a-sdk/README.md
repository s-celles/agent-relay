# Multi-agent A2A — the Python version

Three A2A agents (**coder**, **reviewer**, **summariser**), one relay.

**Stack:** the Python [`a2a-sdk`](https://github.com/a2aproject/a2a-python) for the
agents *and* the orchestrator, and the [`anthropic`](https://github.com/anthropics/anthropic-sdk-python)
SDK for the brain (the relay serves the Anthropic wire).

The sibling example, [`../multiagent-go-a2a-cli/`](../multiagent-go-a2a-cli),
does the same shape with **no Python at all** — the `a2a` CLI from `a2a-go`
wrapping a shell script. Same architecture, different stack; pick the one that
matches your deploy.

The narrative version of this page — *why* one relay and several agents, rather
than several relays — is [docs/multiagent.md](../../multiagent.md).

```
         ┌── coder      (sonnet) ─┐
task ──▶ │── reviewer   (sonnet) ─│──▶ one-line verdict
         └── summariser (granite4.1:3b, local) ─┘
              each is an A2A server with its own card
              each gets its brain from the relay's /v1/messages
```

A role is nothing but a **system prompt and a model**. That is the point:
specialisation is cheap, and choosing the model per agent is where *a brain per
agent, priced per job* actually shows up.

## Files

| | |
|---|---|
| `agents.py` | the three A2A servers — run it once per role |
| `orchestrator.py` | the A2A **client**: discovers the agents by card, then chains them |

The orchestrator never imports `agents.py`. It is handed three base URLs, reads
each Agent Card to learn what the agent *is*, and delegates over the protocol —
exactly as it would to an agent someone else wrote, in another language, on
another machine. That is what makes this A2A rather than three function calls.

## Run it

The summariser runs on a local Ollama model, so start the relay with the Ollama
route (in the repo root), then drive the example with its own `justfile`:

```sh
just run-hybrid                          # repo root: the relay, on :18082

export RELAY_URL=http://127.0.0.1:18082
export RELAY_TOKEN=$(just print-token)   # from the repo root

# in this directory:
just setup                               # venv + the two SDKs
just agents                              # the three A2A servers, backgrounded
just cards                               # optional: fetch each Agent Card
just run "a function that checks whether a string is a palindrome"
just clean                               # stop the agents
```

`just demo "…"` does `agents` + `run` in one go.

Or without `just`, the same steps by hand:

```sh
python3 -m venv .venv
.venv/bin/pip install 'a2a-sdk[http-server]' anthropic uvicorn

.venv/bin/python agents.py coder       # :9101  (three terminals, or background)
.venv/bin/python agents.py reviewer    # :9102
.venv/bin/python agents.py summarizer  # :9103

.venv/bin/python orchestrator.py "a function that checks whether a string is a palindrome"
```

## What it costs

The relay's own accounting, from the run in the docs:

```
coder      (sonnet)              $0.197
reviewer   (sonnet)              $0.071
summariser (granite4.1:3b, local) $0
                                 ------
                                 $0.268
```

The summariser is free **and offline** — no data leaves the machine. That is the
whole argument for putting a relay under a multi-agent system: the cheap step is
cheap on purpose, and you can *see* it (`/v1/metrics`, or the `request usage`
lines in the relay's log).

## Four things the `a2a-sdk` docs do not make obvious

All against **a2a-sdk 1.1.0** — each cost a wrong first attempt, so they are
written down.

| | |
|---|---|
| **server extras** | the server side needs `a2a-sdk[http-server]` — plain `a2a-sdk` has no `sse_starlette`, and `a2a.server.routes` fails to import |
| **`create_from_url` is async** | `client = await factory.create_from_url(base)` — it is a coroutine, not a factory call |
| **no `client.get_card()`** | resolve the card yourself: `await A2ACardResolver(http, base).get_agent_card()`, then `factory.create(card)` |
| **`GetTaskRequest(id=…)`** | not `name="tasks/<id>"` — the field is `id` (plus `tenant`, `history_length`) |

## Where to go next

- **Give an agent tools.** These three only talk. An agent that *acts* sends
  `tools[]` and runs the [client-tool loop](../../api.md#client-defined-tools) —
  the relay serves it on both wires.
- **Route by difficulty, not by hand.** Here each agent's model is fixed. A triage
  agent (local, free) could decide which brain each task deserves.
- **Use a real framework.** This is an example, not an orchestration engine. If
  you want persistence, retries, planning and human-in-the-loop, reach for one
  (Google ADK has first-class A2A support) and point its agents at the relay
  exactly as these three do.
