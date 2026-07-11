#!/usr/bin/env python3
"""A2A interoperability check: drive the relay with the official *Python* SDK.

This is not part of the Go test suite, and it is deliberately not in CI — it
spends real tokens against a real subscription (see docs/interop.md).

What it is for: the Go tests were written by the same hand as the Go server,
against the same reading of the specification. If that reading is wrong, they
agree with each other and pass anyway. This script removes that circularity by
driving the relay with an implementation nobody here wrote — the official
Python A2A SDK — which knows nothing about the relay beyond one base URL, and
discovers everything else from the Agent Card, exactly as a peer would.

Usage:

    python3 -m venv .venv && .venv/bin/pip install 'a2a-sdk>=1.0' httpx

    RELAY_URL=http://127.0.0.1:18082 \
    RELAY_TOKEN=<caller token> \
    RELAY_AGENTIC_TOKEN=<agentic token> \
        .venv/bin/python docs/interop/a2a_interop.py

The relay must be running with RELAY_A2A_ENABLED=true. RELAY_AGENTIC_TOKEN is
optional: without it, the agentic checks are skipped and the rest still runs.
"""

import asyncio
import os
import sys

import httpx
from a2a.client import A2ACardResolver, ClientConfig, ClientFactory
from a2a.types.a2a_pb2 import GetTaskRequest, Message, Part, Role, SendMessageRequest

BASE = os.environ.get("RELAY_URL", "http://127.0.0.1:18082").rstrip("/")
TOKEN = os.environ.get("RELAY_TOKEN", "")
AGENTIC_TOKEN = os.environ.get("RELAY_AGENTIC_TOKEN", "")

TERMINAL_COMPLETED = "TASK_STATE_COMPLETED"

failures: list[str] = []


def check(label: str, ok: bool, detail: str = "") -> None:
    print(f"  [{'ok' if ok else 'FAIL'}] {label}" + (f" — {detail}" if detail else ""))
    if not ok:
        failures.append(label)


def state_name(task) -> str:
    from a2a.types.a2a_pb2 import TaskState

    return TaskState.Name(task.status.state)


def artifact_text(task) -> str:
    return "".join(
        p.text for art in task.artifacts for p in art.parts if p.text
    )


def artifact_urls(task) -> list[tuple[str, str]]:
    return [
        (p.url, p.media_type)
        for art in task.artifacts
        for p in art.parts
        if p.url
    ]


async def send(client, text: str, context_id: str = "", message_id: str = "m"):
    """Send one message and return the resulting Task."""
    msg = Message(
        message_id=message_id,
        role=Role.ROLE_USER,
        parts=[Part(text=text)],
    )
    if context_id:
        msg.context_id = context_id
    task = None
    async for event in client.send_message(SendMessageRequest(message=msg)):
        if event.HasField("task"):
            task = event.task
    return task


async def main() -> int:
    headers = {"Authorization": f"Bearer {TOKEN}"} if TOKEN else {}

    async with httpx.AsyncClient(headers=headers, timeout=300) as http:
        factory = ClientFactory(ClientConfig(httpx_client=http, streaming=False))

        # 1. Discovery. The client is given a base URL and nothing else: it
        #    must find the endpoint through the Agent Card on its own.
        print("\n[1] Discovery — the SDK reads the Agent Card")
        card = await A2ACardResolver(http, BASE).get_agent_card()
        check("the card parses against the v1.0 model", True, card.name)
        iface = card.supported_interfaces[0]
        check(
            "a JSON-RPC interface is advertised",
            iface.protocol_binding == "JSONRPC",
            f"{iface.protocol_binding} {iface.protocol_version} @ {iface.url}",
        )
        check("streaming is advertised", card.capabilities.streaming)
        skills = [s.id for s in card.skills]
        check("skills are advertised", bool(skills), ", ".join(skills))

        client = factory.create(card)

        # 2. A plain task, answered as an artifact.
        print("\n[2] SendMessage — a chat task")
        task = await send(client, "Reply with exactly: INTEROP OK", message_id="m1")
        check("the task reaches a terminal state", state_name(task) == TERMINAL_COMPLETED,
              state_name(task))
        answer = artifact_text(task).strip()
        check("the answer comes back as an artifact", "INTEROP OK" in answer, repr(answer))
        check("the server generated both ids", bool(task.id and task.context_id))
        context_id = task.context_id

        # 3. GetTask — the task outlives the call that created it.
        print("\n[3] GetTask")
        got = await client.get_task(GetTaskRequest(id=task.id))
        check("the task can be fetched back", got.id == task.id, state_name(got))

        # 4. Context continuity: the same contextId must resume the
        #    conversation, not start a fresh one.
        print("\n[4] Same contextId — does the agent remember?")
        again = await send(
            client,
            "What exact phrase did I ask you to reply with?",
            context_id=context_id,
            message_id="m2",
        )
        check("the context is preserved", again.context_id == context_id)
        recall = artifact_text(again)
        check("the backend session resumed", "INTEROP OK" in recall, repr(recall.strip()[:60]))

        # 5. Agentic task: its files must come back as url artifacts, and be
        #    fetchable out of band — A2A defines no download endpoint.
        if not AGENTIC_TOKEN:
            print("\n[5] Agentic task — SKIPPED (no RELAY_AGENTIC_TOKEN)")
        else:
            print("\n[5] Agentic task — files returned as url artifacts")
            agentic_headers = {
                **headers,
                "X-Agentic-Authorization": f"Bearer {AGENTIC_TOKEN}",
            }
            async with httpx.AsyncClient(headers=agentic_headers, timeout=300) as ahttp:
                afactory = ClientFactory(
                    ClientConfig(httpx_client=ahttp, streaming=False)
                )
                acard = await A2ACardResolver(ahttp, BASE).get_agent_card()
                aclient = afactory.create(acard)
                atask = await send(
                    aclient,
                    "Write a file named interop.txt containing the single word "
                    "OK. Then reply DONE.",
                    message_id="m3",
                )
                check("the agentic task completed",
                      state_name(atask) == TERMINAL_COMPLETED, state_name(atask))

                urls = artifact_urls(atask)
                target = [u for u, _ in urls if u.endswith("interop.txt")]
                check("the file it wrote is an artifact", bool(target),
                      ", ".join(u.rsplit("/", 1)[-1] for u, _ in urls) or "none")

                if target:
                    resp = await http.get(target[0])
                    check("the artifact is fetchable out of band",
                          resp.status_code == 200 and "OK" in resp.text,
                          f"HTTP {resp.status_code}: {resp.text.strip()[:20]!r}")

    print()
    if failures:
        print(f"FAILED: {len(failures)} check(s): {', '.join(failures)}")
        return 1
    print("All interop checks passed.")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(asyncio.run(main()))
    except Exception as exc:  # noqa: BLE001 — a probe: report, do not trace
        print(f"\nERROR: {type(exc).__name__}: {exc}", file=sys.stderr)
        sys.exit(2)
