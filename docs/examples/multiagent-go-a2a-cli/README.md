# Multi-agent A2A — the all-Go version

Three A2A agents (**researcher**, **critic**, **writer**), one relay, **no Python
runtime**. The relay is Go; the agents are the `a2a` CLI from
[`a2a-go`](https://github.com/a2aproject/a2a-go) wrapping a shell script.

The sibling example, [`../multiagent-py-a2a-sdk/`](../multiagent-py-a2a-sdk),
does the same shape with the Python `a2a-sdk` and the Anthropic Python SDK. Same
architecture, different stack — pick the one that matches your deploy.

```
        ┌── researcher (haiku)  ─┐
task ──▶│── critic     (sonnet) ─│──▶ final answer
        └── writer     (sonnet) ─┘
             each is `a2a serve --exec agent.sh`
             each gets its brain from the relay's /v1/messages
```

A role is nothing but a **system prompt and a model** — `agent.sh` is one script
that becomes three identities via `ROLE`. That is the point: specialisation is
cheap, and choosing the model per agent is where *a brain per agent, priced per
job* actually shows up. The researcher gathers on `haiku`; the critic and writer
earn `sonnet`.

## Run it

```sh
just setup                      # install the a2a CLI, check jq/curl
# in the repo root, another terminal:  just run       (the relay, on :18082)
just agents                     # the three A2A agents, backgrounded
just cards                      # optional: resolve each Agent Card
just run "Should a personal HTTP relay bind to 0.0.0.0 by default? Argue it in three bullets."
just clean                      # stop the agents
```

`just demo "…"` does `agents` + `run` in one go.

A loopback relay needs no token. If yours has one, export `RELAY_TOKEN` — the
script adds the header only when it is set.

## What it costs

The relay's own accounting, from the run above:

```
researcher (haiku)   $0.013
critic     (sonnet)  $0.069
writer     (sonnet)  $0.073
                     ------
                     $0.155
```

Read it off `/v1/metrics`, or watch the `request usage` lines in the relay's log.
That is the whole argument for putting a relay under a multi-agent system: the
cheap step is cheap, on purpose, and you can *see* it.

## Three things the `a2a` CLI's help does not tell you

All verified against **a2a-go v2.3.1** — they cost an afternoon, so they are
written down.

| | |
|---|---|
| **`--exec` I/O** | the task arrives on **stdin** (argv is empty); whatever the script prints on **stdout** becomes the reply |
| **`a2a send` output** | it prints a **task envelope**, not the bare reply — the answer is in the *artifacts*. Use `-o json` and pull `.artifacts[].parts[].text` |
| **`--timeout`** | defaults to **30 s**. A real model on a real prompt blows through it and the chain dies at the second agent — while the relay keeps generating, and billing. Pass `--timeout 300s` |

## What each piece proves

- **The agents are real A2A servers.** `a2a serve --exec` wraps a plain script as
  a spec-compliant endpoint with a discoverable card (`just cards`) — reachable by
  any A2A client, in any language, not just this one.
- **The relay is the single brain.** Every role calls `/v1/messages`; the only
  thing that differs is `MODEL`.
- **One language end to end.** The relay and the `a2a` binary are both Go. Good
  fit for a Nix/systemd target where a Python runtime is one dependency too many.

## Upgrade path: the SDK rather than the CLI

Swapping the `serve --exec` shell agents for real Go programs turns this into an
`a2a-go` **SDK** example: implement `a2asrv.AgentExecutor`, serve it with
`a2asrv.NewJSONRPCHandler`, orchestrate with `a2aclient`. The relay's own adapter
(`internal/api/a2a`) is exactly that, and is the closest reference in this repo.

The CLI version here is deliberately the low-dependency entry point: three ports,
one shell script, no compilation.
