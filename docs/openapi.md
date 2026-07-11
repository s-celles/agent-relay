# OpenAPI description

The machine-readable contract: [`openapi.json`](openapi.json) (OpenAPI 3.1).

## What it describes — and what it refuses to

It describes **what the relay adds**, not what it merely proxies.

The request and response bodies of `/v1/messages` and `/v1/chat/completions`
belong to the Anthropic Messages API and the OpenAI Chat Completions API. They
are specified upstream and implemented by the official SDKs — so this document
does **not** restate them. Point an existing SDK at the relay; do not generate a
client from this file. Re-deriving those schemas would buy nothing and create a
second source of truth that silently drifts on every upstream change.

What only the relay defines *is* specified, in full:

- authentication (`Authorization: Bearer`, or the Anthropic-style `x-api-key`);
- the header contract — `X-Agentic-Authorization`, `X-Agentic-Keep-Outputs`,
  `X-Agentic-Outputs`, `X-Relay-Session-Id`, `X-Request-Timeout`,
  `X-Agent-Traces`, and what comes back in `X-Request-Id` /
  `X-Relay-Session-Id`;
- the failure modes that are ours and not the model's — 503 (every backend slot
  taken) and 429 (your quota) with `Retry-After`, 504 (deadline, *not* a backend
  failure) versus 502 (backend failure), 403 (agentic refused);
- the retained-outputs endpoints, including the one an [A2A](a2a.md) artifact's
  `url` part points at.

**Out of scope on purpose:** the A2A surface (`POST /a2a`, the Agent Card). A2A
is already self-describing — its Agent Card *is* the machine-readable contract,
and its normative schema is the A2A protobuf. Describing it a third time here is
precisely how the upstream A2A specification acquired the contradictions
recorded in `upstream-bugs.md`.

## It cannot go stale

An API description that drifts is worse than no description: it lies with
authority. So `internal/server/openapi_test.go` holds this file against the
routes the server actually registers, in both directions —

- every registered route must appear here, unless it is in an explicit
  `outOfScope` list **with its reason**;
- every operation described here must actually be served;
- every `outOfScope` entry must still correspond to a real route.

Add, move or delete an endpoint without touching `openapi.json` and the build
fails.

The tests check *coverage*, not schemas — validating bodies would mean restating
the upstream APIs, which is exactly what this document declines to do.

<div id="swagger"></div>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  window.addEventListener("load", function () {
    // Pages are served with directory URLs (/openapi/), so a bare "openapi.json"
    // would resolve one level too deep. Resolve it against the page instead.
    SwaggerUIBundle({
      url: new URL("../openapi.json", window.location.href).href,
      dom_id: "#swagger",
      supportedSubmitMethods: []
    });
  });
</script>
