#!/usr/bin/env python3
"""Three specialised A2A agents, all backed by ONE agent-relay.

Stack: Python — the `a2a-sdk` package (A2A protocol) and the `anthropic` SDK
(the relay's Anthropic wire). The all-Go sibling is ../multiagent-go-a2a-cli.

The point of a multi-agent system is that the agents differ by **role** — a
coder, a reviewer, a summariser — not by which model they happen to run on. So
the specialisation lives here, in the agents; the relay below them is just the
thing that thinks. One relay, several agents.

Each agent is a real A2A server: its own Agent Card, its own skills, its own
endpoint. The orchestrator discovers them by card and delegates tasks over the
protocol — it never imports this file.

What the relay buys you is *choice of brain per agent*. The summariser runs on a
local Ollama model: free, offline, ~3 s. The coder and reviewer spend the
subscription, because their work is worth it. Same relay, same token, one line
of config apart.

Run all three (each in its own terminal, or with `&`):

    python agents.py coder      # :9101
    python agents.py reviewer   # :9102
    python agents.py summarizer # :9103

Requires a relay on RELAY_URL with the Ollama route for the summariser's model
(`just run-hybrid`), and RELAY_TOKEN set.
"""

import os
import sys

import uvicorn
from anthropic import Anthropic
from starlette.applications import Starlette

from a2a.server.agent_execution import AgentExecutor, RequestContext
from a2a.server.events import EventQueue
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.routes import create_agent_card_routes, create_jsonrpc_routes
from a2a.server.tasks import InMemoryTaskStore
from a2a.types.a2a_pb2 import (
    AgentCapabilities,
    AgentCard,
    AgentInterface,
    AgentSkill,
    Message,
    Part,
    Role,
)

RELAY_URL = os.environ.get("RELAY_URL", "http://127.0.0.1:18082")
RELAY_TOKEN = os.environ.get("RELAY_TOKEN", "")

# The relay is the one brain-provider. Each agent picks a model from it.
relay = Anthropic(base_url=RELAY_URL, api_key=RELAY_TOKEN)


# --- the three roles ---------------------------------------------------------
#
# A role is a system prompt + a model. That is the whole of an agent's identity
# here — which is the point: specialisation is cheap, and it is where the value
# of a multi-agent system actually lives.

ROLES = {
    "coder": {
        "port": 9101,
        "model": "sonnet",
        "skill": "write-code",
        "description": "Writes a small, self-contained piece of code to a spec.",
        "system": (
            "You are a careful Python programmer. Given a request, reply with "
            "ONE self-contained function and nothing else — no prose, no "
            "markdown fences, no example usage. Include a docstring."
        ),
    },
    "reviewer": {
        "port": 9102,
        "model": "sonnet",
        "skill": "review-code",
        "description": "Reviews code and reports concrete defects.",
        "system": (
            "You are a code reviewer. Given code, list its concrete defects as "
            "short bullets: correctness first, then edge cases. If the code is "
            "sound, say so in one line. Be specific; do not restate the code."
        ),
    },
    "summarizer": {
        # A cheap, local, offline model — this work does not deserve the
        # subscription. `just run-hybrid` routes this name to Ollama.
        "port": 9103,
        "model": "granite4.1:3b",
        "skill": "summarize",
        "description": "Condenses a result into one sentence.",
        "system": (
            "Summarise what you are given in ONE short sentence. No preamble."
        ),
    },
}


class RelayAgent(AgentExecutor):
    """An A2A agent whose thinking is delegated to the relay."""

    def __init__(self, role: dict):
        self.role = role

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        question = context.get_user_input()

        resp = relay.messages.create(
            model=self.role["model"],
            max_tokens=1500,
            system=self.role["system"],
            messages=[{"role": "user", "content": question}],
        )
        answer = "".join(b.text for b in resp.content if b.type == "text")

        await event_queue.enqueue_event(
            Message(
                message_id=f"{self.role['skill']}-reply",
                role=Role.ROLE_AGENT,
                parts=[Part(text=answer)],
            )
        )

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        raise NotImplementedError("this example does not support cancellation")


def build_card(name: str, role: dict) -> AgentCard:
    """The agent's public identity: what a peer reads before talking to it."""
    return AgentCard(
        name=f"{name} agent",
        description=role["description"],
        version="1.0.0",
        supported_interfaces=[
            AgentInterface(
                url=f"http://127.0.0.1:{role['port']}/a2a",
                protocol_binding="JSONRPC",
                protocol_version="1.0",
            )
        ],
        capabilities=AgentCapabilities(streaming=False),
        default_input_modes=["text/plain"],
        default_output_modes=["text/plain"],
        skills=[
            AgentSkill(
                id=role["skill"],
                name=role["skill"],
                description=role["description"],
                tags=[name],
            )
        ],
    )


def main() -> int:
    if len(sys.argv) != 2 or sys.argv[1] not in ROLES:
        print(f"usage: agents.py [{' | '.join(ROLES)}]", file=sys.stderr)
        return 2
    if not RELAY_TOKEN:
        print("RELAY_TOKEN is required", file=sys.stderr)
        return 2

    name = sys.argv[1]
    role = ROLES[name]
    card = build_card(name, role)

    handler = DefaultRequestHandler(
        agent_executor=RelayAgent(role),
        task_store=InMemoryTaskStore(),
        agent_card=card,
    )
    app = Starlette(
        routes=create_agent_card_routes(card) + create_jsonrpc_routes(handler, "/a2a")
    )

    print(f"{name} agent (model: {role['model']}) on :{role['port']}")
    uvicorn.run(app, host="127.0.0.1", port=role["port"], log_level="warning")
    return 0


if __name__ == "__main__":
    sys.exit(main())
