# API vs relay: what the CLI cannot do

The relay speaks the Anthropic Messages wire format, but it is **not** the
Anthropic API: behind it sits the `claude` CLI, an agent with its own loop,
not a raw model endpoint. Some API features translate cleanly, some degrade,
and some are structurally impossible. This page is the honest map.

## Quick reference

| API feature | Through the relay | Why |
|---|---|---|
| Text in / text out, streaming, usage | ✅ works | Native fit |
| Structured history (`tool_use`/`tool_result` blocks) | ✅ works | Flattened into the transcript |
| Images & PDFs (base64 blocks) | ✅ bridged | Materialized to files, viewed via the CLI's Read tool |
| System prompt | ⚠️ degraded | Passed via `--system-prompt`, but the CLI layers its own behavior |
| `max_tokens` | ⚠️ not enforced | No CLI flag; accepted + one-time warning |
| `stop_reason` richness | ⚠️ reduced | Only `end_turn`/`tool_use`; no `max_tokens`/`refusal`/`pause_turn` |
| Client-defined tools (`tools[]`) | ✅ works | Bridged over MCP; the subprocess parks between turns |
| Structured outputs (JSON schema, `strict`) | ❌ absent | No CLI equivalent; prompt engineering only |
| Assistant prefill / exact turn replay | ❌ absent | History is a text approximation — but see session continuity below |
| Multi-turn continuity | ✅ via `X-Session-Id` | The backend keeps its own conversation; no transcript replay needed |
| `temperature`, `top_p`, `top_k`, `stop_sequences` | ❌ dropped (signaled) | No CLI equivalents; the relay warns rather than ignoring silently |
| Thinking/effort control per request | ❌ absent | The CLI decides internally |
| Prompt caching control (`cache_control`) | ❌ absent | The CLI manages its cache; no client visibility |
| `count_tokens`, Batches, Files API, `/v1/models` | ❌ absent | No CLI equivalents |

## Structurally impossible

These cannot be fixed inside the relay — the CLI simply has no raw-model
mode. Resolving them would require either an upstream CLI feature or a
backend that fronts the raw API (changing the billing model from subscription
to API key).

- **Guaranteed structured outputs.** No `output_config.format`, no
  `strict: true`. Prompting for JSON works as well as it works — nothing
  validates the result.
- **Exact conversation replay.** The API is stateless with token-exact
  history (including thinking blocks and prefills). The relay flattens
  multi-turn history into a framed text transcript: behaviorally close, but
  an approximation.

## Bridged, with caveats

- **Client-defined tools**: `tools[]` works — the relay exposes your tools to
  the CLI over MCP and parks the subprocess between turns, so the standard
  Messages API tool loop (and the official SDKs) work unmodified. See
  [HTTP API — Client-defined tools](api.md#client-defined-tools). The
  caveats: a parked conversation holds a concurrency slot until you return a
  result (or the request timeout tears it down), and `tool_choice` is not
  enforced.

- **Vision and PDFs**: base64 `image`/`document` blocks are accepted and
  materialized into a per-request ephemeral working directory; the CLI's
  read-only Read tool views them (see [HTTP API — Attachments](api.md#attachments-images-and-pdfs)).
  The caveat: viewing is *model-mediated* — the model follows the file
  reference (it reliably does), but it is not the structural guarantee of
  native API vision.

## Degraded

- **System prompt fidelity.** `--system-prompt` is honored, but the
  subprocess is still Claude Code: it may pick up context from its working
  directory (mitigated: attachment-carrying requests run in a clean
  ephemeral directory; set `RELAY_CLAUDE_WORKDIR` for the rest) and keeps
  agent-flavored behaviors.
- **Stop reasons.** The stream-json output does not distinguish
  `max_tokens`, `refusal`, or `pause_turn`; the relay reports
  `end_turn`/`tool_use` plus in-band errors.
- **Latency.** Every request pays a CLI startup (a Node process) on top of
  inference. Interactive chat is fine; latency-sensitive pipelines will feel
  it.
- **Throughput.** Bounded by your subscription's usage windows, not API rate
  tiers, with `RELAY_MAX_CONCURRENT` as the local throttle.

## The practical dividing line

Use the relay for **text-to-text and tool-calling on your own
subscription**: chat clients, summarization, classification, LLM-as-judge
loops, personal batch jobs, prompt-orchestrated harnesses, agent loops
driving your own tools — plus images/PDFs via the attachment bridge. Reach
for a real API key the moment you need schema-guaranteed outputs, sampling
control, or caching economics: no relay-side work can close those gaps
against the CLI; a raw-API backend could, at the cost of API billing.
