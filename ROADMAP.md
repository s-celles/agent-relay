# Roadmap

What is known to be missing or deferred, relative to the design document
(`spec.md`, untracked) and to gaps found while using the relay. Items move to
[CHANGELOG.md](CHANGELOG.md) when they ship.

## Near term (correctness & auditability)

- [ ] **Log agentic requests individually.** The design calls agentic use
  "opt-in, explicit, logged", but only startup is logged today. Each agentic
  request should leave a log line (request-id, agentic flag) as an audit
  trail.
- [ ] **Handle `max_tokens` honestly.** Both wire formats accept it, but the
  `claude` CLI has no equivalent flag, so it is silently ignored. Document
  the limitation and log a warning when a client sets it.
- [ ] **Back-port resolved decisions into `spec.md`.** OD-1/3/4 into §9, and
  the answers implemented since: DQ-1 (defensive stream-json parsing), DQ-2
  (model map with pass-through), DQ-4 (minimal JSON metrics), the
  REQ-EXEC-04 ephemeral-workdir mechanism, and the REQ-EXEC-06 per-request
  authorization design (`X-Agentic-Authorization` + `RELAY_AGENTIC_TOKENS`).
- [ ] **Add the requirements document to the repo.** `spec.md` derives from
  `agent-relay-spec.md` (requirements v0.1), which is not in the tree;
  requirement IDs it cites (REQ-EXEC-03, REQ-NET-03, REQ-CFG-01…) cannot be
  audited without it.

## Undecided (needs a design decision first)

- [ ] **Config file overlay (DQ-3).** The design says "env-first with an
  optional file overlay"; only env exists. Either implement a TOML overlay
  or amend the spec to declare env-only as final.
- [ ] **Retrieving agentic outputs.** Ephemeral workdirs delete whatever the
  agent produced; anything to keep must fit in the response text. If real
  workflows need artifacts back, design a retrieval mechanism (inline
  base64, follow-up endpoint, …).

## Later (v0.2+ candidates)

- [x] **Structured content (wire level).** `tool_use`/`tool_result` blocks,
  `tools[]`, and `tool_choice` now decode on both wire formats; responses can
  carry `tool_use` blocks with streaming `input_json_delta`; the claude
  backend flattens structured history into its transcript. Image blocks are
  still rejected.
- [ ] **Client-tool execution.** Even with the wire support above, the
  claude CLI has no raw tool-calling mode (it runs its own agent loop), so
  requests with `tools[]` are rejected with 400 on the claude backend and
  agentic clients (Claude Agent SDK, Claude Code) still cannot use the relay
  as their backend. Unblocking this requires either an upstream CLI feature
  or a backend that fronts the raw model API.
- [ ] **Second backend (Gemini or Codex).** The registry seam exists
  (REQ-BK-03) but is unexercised; a second adapter would prove it.
- [ ] **Model-map routing across backends.** When a second backend lands,
  route requests by logical model name instead of the global
  `RELAY_BACKEND` selection (resolves DQ-2 fully).
- [ ] **NixOS packaging.** `docs/deployment.md` sketches `buildGoModule` +
  a systemd module; nothing declarative is in-tree.
- [ ] **Native multi-turn conversations.** History is currently flattened
  to a `Human:`/`Assistant:` transcript on stdin — an approximation the CLI
  imposes.

## Wire-compatibility polish

- [ ] Signal (rather than silently drop) unsupported sampling parameters
  (`temperature`, `top_p`, `stop_sequences`).
- [ ] Include usage in OpenAI streaming responses
  (`stream_options: {"include_usage": true}` convention).
- [ ] Remove or use the `EventUsage` neutral event kind (defined, never
  emitted standalone).

## Non-goals (deliberate)

- **TLS termination.** The relay targets loopback and trusted private
  networks (Tailscale); put a reverse proxy in front if you need TLS.
- **Multi-tenant service.** One operator, one subscription, personal use —
  see the [terms-of-service disclaimer](README.md#disclaimer--terms-of-service).
