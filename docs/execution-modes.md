# Execution modes: inference vs agentic

The relay wraps the `claude` CLI, and the CLI is not just a text generator —
it is an *agent* with tools: it can write files, edit code, and run shell
commands. Which of those capabilities a relayed request gets is the single
most security-relevant decision in the whole system, so the relay makes it an
explicit, layered choice between two modes.

## Inference mode (the default)

**What it is**: the CLI used purely as a text-completion engine. A prompt
goes in on stdin, tokens come out on stdout, nothing else happens.

**How it is enforced** — not by asking the model nicely, but structurally:

- The subprocess is spawned with **no permission flags**. The CLI's tools
  (Write, Edit, Bash, …) all require a permission grant before executing,
  and in non-interactive `-p` mode there is no human to grant one. A tool
  call therefore dead-ends: the model can only *describe* what it would have
  done.
- The environment is sanitized (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`,
  `CLAUDECODE`, plus `RELAY_ENV_DENY`), so the subprocess can neither loop
  back through the relay nor read operator secrets.
- The working directory is the static `RELAY_CLAUDE_WORKDIR` (or the relay's
  own, if unset — see the caveat below).

**The guarantee**: an authenticated caller can obtain text and spend tokens.
Nothing more. This is what the relay serves by default, and what every
Anthropic/OpenAI-compatible client gets out of the box.

**Two caveats worth knowing**:

- The model may *claim* it performed an action ("file created ✓") — it
  cannot have. Treat action claims from inference mode as fiction.
- The CLI picks up context from its working directory (project files,
  directory listing). If the relay runs inside a code repository, answers
  may reflect that repository's content. Set `RELAY_CLAUDE_WORKDIR` to a
  neutral directory if that matters to you.

## Agentic mode (explicit opt-in)

**What it is**: the CLI allowed to actually act — create and edit files,
and, depending on the flags you grant, run commands. The relay becomes a
remote *agent execution* service, not just an inference proxy.

**How a request becomes agentic** — four independent layers, each of which
can say no:

1. **Operator opt-in**: `RELAY_AGENTIC_ENABLED=true`. Without it, agentic
   requests are rejected and the startup log stays quiet. With it, the relay
   logs a loud warning at startup.
2. **Startup guards**: agentic on a non-loopback bind refuses to start
   unless per-request authorization is configured; per-request authorization
   refuses to start without agentic tokens.
3. **Per-request authorization** (when `RELAY_AGENTIC_PER_REQUEST_AUTHZ=true`):
   the request must carry `X-Agentic-Authorization: Bearer <token>` matching
   `RELAY_AGENTIC_TOKENS` — a *second* credential, deliberately distinct
   from the caller token. Absent header → the request silently runs in
   inference mode instead. Wrong header → 403, before any subprocess exists.
4. **Backend re-check**: even a request the server marked agentic is refused
   by a backend that was not configured for agentic execution.

**What an agentic request gets**:

- The operator-chosen permission flags from `RELAY_AGENTIC_ARGS` (e.g.
  `--permission-mode,acceptEdits` for file edits only, up to
  `--dangerously-skip-permissions` for everything — choose deliberately).
- An **ephemeral working directory** of its own, created under
  `RELAY_CLAUDE_WORKDIR` and deleted when the request ends. Concurrent
  agentic requests cannot see each other's files, and no state survives
  between requests. Corollary: files the agent produces vanish with the
  directory — anything you want to keep must be returned in the response
  text.

**Audit trail**: agentic execution is not just opt-in but logged, per
request. Every request that is actually authorized to run agentically emits
exactly one structured log line (`agentic request authorized`, level Info)
carrying the request id — the same value returned to the caller in the
`X-Request-Id` response header — and the request path, so agentic activity
can be correlated with responses and reviewed after the fact. Rejected
agentic attempts (agentic disabled, or an invalid `X-Agentic-Authorization`
credential) are logged at level Warn (`agentic request denied`) with the
same correlation fields plus a `reason`, in addition to incrementing the
`agentic_denied` metric. One caveat: the id echoes the caller-suppliable
`X-Request-Id` request header (generated server-side only when absent), so
treat it as a correlation aid, not a tamper-proof identifier.

**What agentic mode does *not* provide**: a sandbox. The subprocess runs as
the relay's OS user, with that user's privileges, network access, and home
directory. `acceptEdits` confines *writes* to the ephemeral directory, but
broader grants (Bash, bypass) mean arbitrary code execution as that user.
Run agentic relays in a container (or a dedicated unprivileged user) and
keep them on loopback or a trusted private network.

## Side-by-side

| | Inference (default) | Agentic (opt-in) |
|---|---|---|
| Purpose | prompt → text | prompt → actions + text |
| CLI permission flags | none, ever | `RELAY_AGENTIC_ARGS`, per authorized request |
| Side effects on host | none | file writes; commands if granted |
| Working directory | static (`RELAY_CLAUDE_WORKDIR` or inherited) | ephemeral per request, auto-deleted |
| Extra credential | — | `X-Agentic-Authorization` (with per-request authz) |
| Non-loopback bind | allowed with caller tokens | only with per-request authz |
| Failure mode if abused | token spend | code execution as the relay user |
| Sensible deployment | anywhere the startup guards allow | container / dedicated user, private network |

## Choosing

Use inference mode unless you specifically need side effects; it is the
entire reason the relay is safe to expose on a Tailnet. Reach for agentic
mode only for workflows that need real actions (scaffolding files, running
checks), grant the narrowest `RELAY_AGENTIC_ARGS` that works, and treat the
agentic token like a root password: whoever holds it can make your machine
do things.
