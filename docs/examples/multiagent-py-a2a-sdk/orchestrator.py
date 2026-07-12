#!/usr/bin/env python3
"""An A2A orchestrator: discovers three agents by their cards, then chains them.

Stack: Python — the `a2a-sdk` package (A2A protocol) and the `anthropic` SDK
(the relay's Anthropic wire). The all-Go sibling is ../multiagent-go-a2a-cli.

It knows nothing about the agents beyond their base URLs. It reads each Agent
Card to learn what the agent is and where to reach it, then delegates over A2A —
exactly as it would to an agent someone else wrote, on another machine, in
another language. That is the whole point of the protocol.

The chain:

    task ──▶ coder ──▶ code ──▶ reviewer ──▶ review ──┐
                │                                     │
                └──────────────▶ summarizer ◀─────────┘
                                     │
                                     ▼
                              one-line verdict

Run the three agents first (see agents.py), then:

    RELAY_TOKEN=<token> python orchestrator.py "a function that reverses a string"
"""

import asyncio
import os
import sys

import httpx

from a2a.client import A2ACardResolver, ClientConfig, ClientFactory
from a2a.types.a2a_pb2 import Message, Part, Role, SendMessageRequest

AGENTS = {
    "coder": "http://127.0.0.1:9101",
    "reviewer": "http://127.0.0.1:9102",
    "summarizer": "http://127.0.0.1:9103",
}


async def discover(http: httpx.AsyncClient, base: str):
    """Read an agent's card and build a client for it — pure A2A discovery."""
    card = await A2ACardResolver(http, base).get_agent_card()
    client = ClientFactory(ClientConfig(httpx_client=http, streaming=False)).create(card)
    return card, client


async def ask(client, text: str, msg_id: str) -> str:
    """Send one A2A task and return the agent's answer."""
    request = SendMessageRequest(
        message=Message(
            message_id=msg_id, role=Role.ROLE_USER, parts=[Part(text=text)]
        )
    )
    out: list[str] = []
    async for event in client.send_message(request):
        if event.HasField("message"):
            out += [p.text for p in event.message.parts if p.text]
        elif event.HasField("task"):
            out += [
                p.text
                for artifact in event.task.artifacts
                for p in artifact.parts
                if p.text
            ]
    return "".join(out).strip()


async def main() -> int:
    if not os.environ.get("RELAY_TOKEN"):
        print("RELAY_TOKEN is required (the agents need it, not this script)",
              file=sys.stderr)
    spec = sys.argv[1] if len(sys.argv) > 1 else "a function that reverses a string"

    async with httpx.AsyncClient(timeout=300) as http:
        # 1. DISCOVERY — the orchestrator learns who these agents are from
        #    their cards alone. Nothing here is hard-coded about their roles.
        print("Discovering agents…")
        agents = {}
        for name, base in AGENTS.items():
            card, client = await discover(http, base)
            agents[name] = client
            print(f"  {card.name:18} {card.skills[0].id:14} {card.description}")

        # 2. DELEGATE — each agent does the one thing it is for.
        print(f"\n[1] coder      ← {spec!r}")
        code = await ask(agents["coder"], f"Write {spec}.", "task-1")
        print("\n".join("      " + line for line in code.splitlines()[:12]))

        print("\n[2] reviewer   ← the code above")
        review = await ask(agents["reviewer"], f"Review this code:\n\n{code}", "task-2")
        print("\n".join("      " + line for line in review.splitlines()[:12]))

        # 3. The cheap agent gets the cheap job — it runs on a local model, so
        #    this step costs nothing and never leaves the machine.
        print("\n[3] summarizer ← both (local model — free)")
        verdict = await ask(
            agents["summarizer"],
            f"Task: {spec}\n\nCode:\n{code}\n\nReview:\n{review}\n\n"
            "In one sentence: is this good to merge?",
            "task-3",
        )
        print(f"      {verdict}")

    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
