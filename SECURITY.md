# Security policy & threat model

Read this before you expose the relay to anything but your own loopback.
agent-relay turns a personal agent CLI into a network service; that is
useful, and it is also a concentration of risk. This document states plainly
what the relay defends against, what it does **not**, and what you must do
yourself.

## Reporting a vulnerability

Found a security issue in the relay itself — an authentication bypass, a
startup guard that does not hold, a path traversal, a sandbox escape? Report
it privately via
[GitHub Security Advisories](https://github.com/s-celles/agent-relay/security/advisories/new)
rather than opening a public issue. Please include a reproduction and the
version or commit.

Security fixes land on the latest released minor version; there is no
long-term support branch.

## What the relay is, in security terms

A process that:

1. authenticates HTTP callers with a bearer token,
2. spawns the `claude` CLI once per request as **your** OS user, on **your**
   subscription,
3. optionally lets that subprocess write files and run commands (agentic
   mode).

Every one of those is a lever an attacker would want. The defaults are safe;
the interesting deployments are the ones that relax them.

## Assets an attacker is after

- **Your subscription** — free inference on your account, until you hit
  limits or get flagged. Reachable by anyone who obtains a caller token.
- **Code execution as your user** — in agentic mode with broad permissions, a
  caller (or a prompt-injection payload) can run commands with your
  privileges, your network access, and your home directory.
- **Your files and secrets** — anything your OS user can read, including
  other credentials on the host, if agentic execution is granted or the
  working directory is poorly chosen.
- **The relay as a pivot** — a foothold on a machine inside your private
  network (Tailnet, LAN).

## Trust boundaries

```text
[ untrusted network ]
        |  HTTP + bearer token          <- boundary 1: authentication
        v
[ relay process ]  -- runs as your OS user
        |  spawn, per request           <- boundary 2: agentic gate + permissions
        v
[ claude CLI subprocess ]
        |  tools (Write / Bash / ...)   <- boundary 3: OS-level, NOT enforced by the relay
        v
[ your host: files, network, secrets ]
```

- **Boundary 1** is the relay's job and it holds: constant-time token
  comparison, rejection before any subprocess is spawned, and a startup guard
  that refuses a non-loopback bind without tokens.
- **Boundary 2** is the relay's job: agentic execution is off by default,
  gated by an operator flag, startup guards, an optional second credential,
  and a backend re-check.
- **Boundary 3 is NOT the relay's.** Once the CLI holds a permission grant,
  what it can touch is bounded by the **operating system**, not by
  agent-relay. This is the most important sentence in this document.

## What the relay defends against

- **Unauthenticated access on an exposed interface.** By construction there is
  no configuration in which an unauthenticated caller on a non-loopback
  interface reaches a backend — the startup guard refuses to boot instead.
- **Accidental agentic execution.** Off unless explicitly enabled; on a
  non-loopback bind it additionally requires per-request authorization to
  boot at all.
- **Cross-request contamination.** Each agentic request runs in its own
  ephemeral working directory, deleted when the request ends.
- **Recursive loops and host-credential leaks into the subprocess.** The
  child environment is sanitized (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`,
  `CLAUDECODE`, plus any operator deny list — consider adding
  `ANTHROPIC_API_KEY`).
- **Runaway or abandoned subprocesses.** Per-request timeout, a concurrency
  cap that answers 503 instead of fork-bombing the host, and a process-group
  kill on cancellation.

## What the relay does NOT defend against — you own these

- **Transport security.** The relay speaks plain HTTP. Tokens and prompts
  cross the wire in the clear. Off loopback you **must** front it with a
  TLS-terminating reverse proxy (see below).
- **Sandboxing agentic execution.** `--permission-mode acceptEdits` confines
  *file writes* to the ephemeral directory, but any broader grant (Bash,
  `--dangerously-skip-permissions`) is arbitrary code execution as your user.
  The relay does not containerize, drop privileges, or restrict egress. Run
  it in a container or as a dedicated unprivileged user if you grant broad
  agentic permissions.
- **Prompt injection.** In agentic mode the model chooses which tools to call
  based on its input — and that input may include a file it read or a page it
  fetched. The relay cannot distinguish a legitimate instruction from an
  injected one; OS-level containment (boundary 3) is your only backstop.
- **Token issuance, expiry, and rotation.** The relay compares tokens; it does
  not manage their lifecycle.
- **Per-caller rate limiting — partially.** `RELAY_RATE_LIMIT_RPM` now bounds
  sustained requests per minute per caller (429 + `Retry-After`), which caps
  the rate at which one caller can spend your subscription. It is **off by
  default**, it is a request-rate quota rather than a token/cost budget, and
  it is in-process (no shared state across replicas). A reverse proxy remains
  the place for edge-level throttling.
- **Tamper-proof auditing.** Agentic requests are logged, but the correlation
  id echoes a caller-suppliable header and there is no append-only audit
  store.
- **Discovery, if you enable A2A.** `RELAY_A2A_ENABLED=true` publishes an
  **unauthenticated** Agent Card at `/.well-known/agent-card.json` — that is
  what a card is for: a peer reads it before it holds any credential. The card
  names the models you serve and states whether this host can run an agent, so
  it tells an unauthenticated scanner that a Claude subscription and possibly a
  code-executing agent live here. The `/a2a` endpoint itself is authenticated
  and rate-limited like every other inference endpoint. A2A is off by default
  for exactly this reason; turn it on deliberately, and keep it behind the same
  reverse proxy and private network as the rest.

## Why a reverse proxy is not optional off loopback

The relay deliberately ships without TLS, without per-caller quotas, and with
nothing but a static bearer token — that is what keeps its audit surface
small enough to read in an afternoon. The intended shape is a reverse proxy
(Caddy, nginx, or `tailscale serve`) doing the parts a proxy does better:

- **TLS termination.** Without it, a static bearer token on plain HTTP is
  sniffable by anyone on the path — and that token is a key to your
  subscription, and in agentic mode to your machine. This alone justifies the
  proxy.
- **A real rate limit**, per client IP or token, bounding subscription abuse.
- **Request size limits, timeouts, connection caps** at the edge.
- **Optional mTLS or a second factor** for higher-value deployments.

Even on a Tailnet — where WireGuard already authenticates and encrypts the
network, so bare HTTP is defensible — a proxy still buys you rate limiting and
one place to revoke access.

Treat these as the only two supported postures: **loopback only**, or
**behind a TLS-terminating, rate-limiting proxy**. A plain-HTTP relay bound
directly to a public or semi-public interface is not one of them.

## Account sharing: the risk beyond terms of service

The terms-of-service disclaimer covers the contractual point — a consumer
subscription is personal, and sharing or reselling access through the relay
violates it. The **security** consequences are worse than the contractual
ones and deserve their own statement:

- **A shared token is a shared identity.** Everyone holding a caller token
  acts *as you*, on your account. You cannot separate their traffic from
  yours, attribute misuse, or revoke one holder without rotating the token
  for everyone.
- **Their prompts run on your reputation.** If someone uses your relay to
  produce content that trips safety systems or breaches the usage policy, it
  is *your* account that gets rate-limited, flagged, or suspended — the
  provider sees a single identity.
- **Blast radius scales with the audience.** A token used by one script you
  wrote is low-risk. The same token handed to a team, embedded in a shared
  app, or shipped in a client bundle is an open door to your subscription
  and — in agentic mode — to your host.
- **You inherit everyone's operational security.** One holder who commits the
  token to a public repo, or runs a client on a compromised laptop, has
  exposed your account and your machine. You are as safe as the least careful
  token holder.

The relay assumes **one operator, one subscription, personal use on a trusted
private network**. It has no notion of tenants, per-user quotas, or per-user
revocation, because building those safely is a different project. Wanting
them is the signal to move to an API key with proper per-consumer
credentials — not to hand your subscription token to more people.

## Deployment postures, ranked

| Posture | Transport | Agentic | Verdict |
|---|---|---|---|
| Loopback, inference only | n/a | off | Safest; the default |
| Tailnet, inference, behind a proxy | WireGuard (+TLS) | off | Fine for personal multi-device use |
| Tailnet, agentic `acceptEdits`, containerized | WireGuard (+TLS) | writes only, sandboxed | Acceptable for personal automation |
| Public interface | TLS proxy mandatory | any | Only with a hardened proxy; you own every risk above |
| Agentic with broad permissions, not containerized | any | Bash / bypass | **Don't.** Arbitrary code execution as your user |
| Token shared with others | any | any | **Don't.** See account sharing |
