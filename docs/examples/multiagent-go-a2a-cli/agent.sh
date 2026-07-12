#!/usr/bin/env bash
# One role-agent, backed by the relay.
#
# Its entire identity is a system prompt and a model. That is the point:
# specialisation is cheap, and choosing the model per agent is where "a brain per
# agent, priced per job" shows up — a small model researches, a stronger one
# critiques and writes.
#
# `a2a serve --exec` runs this once per task. The contract, verified against
# a2a-go v2.3.1 (`a2a help serve` does not state it, so it was probed):
#
#   the task text arrives on STDIN — not as an argument (argv is empty)
#   whatever we print on STDOUT becomes the agent's reply (an A2A artifact)
#
set -euo pipefail

role="${ROLE:?set ROLE=researcher|critic|writer}"
RELAY_URL="${RELAY_URL:-http://127.0.0.1:18082}"

case "$role" in
  researcher)
    MODEL="haiku"   # cheap: gathering is not where the money should go
    SYSTEM="You are a meticulous researcher. For the request, list the key facts, the open questions, and what would settle them. Be terse — bullets, no preamble." ;;
  critic)
    MODEL="sonnet"  # judgement is worth the subscription
    SYSTEM="You are a sharp critic. Given a request and researcher notes, name the weakest claims and exactly what would falsify each. Be specific and brief." ;;
  writer)
    MODEL="sonnet"
    SYSTEM="You are a clear writer. Turn the notes and critique you are given into a tight final answer to the request. No preamble, no meta-commentary." ;;
  *)
    echo "unknown role: $role" >&2; exit 2 ;;
esac

task="$(cat)"

# Build an Anthropic Messages payload and call the relay. The api key header is
# added only when RELAY_TOKEN is set, so a tokenless loopback relay just works.
jq -n --arg m "$MODEL" --arg s "$SYSTEM" --arg t "$task" \
  '{model: $m, max_tokens: 1024, system: $s, messages: [{role: "user", content: $t}]}' \
| curl -sS "$RELAY_URL/v1/messages" \
    ${RELAY_TOKEN:+-H "x-api-key: $RELAY_TOKEN"} \
    -H "content-type: application/json" \
    --data @- \
| jq -r '[.content[]? | select(.type == "text") | .text] | join("")'
