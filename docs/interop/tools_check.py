#!/usr/bin/env python3
"""Client-tool check: drive the relay's MCP tool bridge with the official
Anthropic SDK, exercising several distinct tool_use patterns.

Like the A2A interop check, this is NOT part of the Go suite and NOT in CI: it
spends real tokens against a real subscription. It exists because the behaviour
it verifies — does the model actually *call* the caller's tools, rather than its
own native ones — cannot be reproduced with the stub CLI the Go tests use. A
regression here (the exact "model narrates instead of calling the tool" bug)
would pass every unit test and only surface against the real CLI.

The SDK runs the standard Messages tool loop unmodified; the relay is just its
base_url. If these pass, an agent client (OpenCode, LangChain, …) works too.

Usage:

    python3 -m venv .venv && .venv/bin/pip install anthropic
    RELAY_URL=http://127.0.0.1:18082 RELAY_TOKEN=<token> \
        .venv/bin/python docs/interop/tools_check.py
"""

import json
import os
import sys

from anthropic import Anthropic

BASE = os.environ.get("RELAY_URL", "http://127.0.0.1:18082")
TOKEN = os.environ.get("RELAY_TOKEN", "")
MODEL = os.environ.get("RELAY_MODEL", "haiku")

client = Anthropic(base_url=BASE, api_key=TOKEN)
failures: list[str] = []


def check(label: str, ok: bool, detail: str = "") -> None:
    print(f"  [{'ok' if ok else 'FAIL'}] {label}" + (f" — {detail}" if detail else ""))
    if not ok:
        failures.append(label)


def run_loop(tools, user_msg, execute, max_turns=6):
    """Run the standard Messages tool loop, executing tools with `execute`.

    Returns (calls, final_text): every (name, input) the model asked for, and
    the assistant's final text once it stops calling tools.
    """
    messages = [{"role": "user", "content": user_msg}]
    calls = []
    for _ in range(max_turns):
        resp = client.messages.create(
            model=MODEL, max_tokens=400, tools=tools, messages=messages
        )
        if resp.stop_reason != "tool_use":
            text = "".join(b.text for b in resp.content if b.type == "text")
            return calls, text
        messages.append({"role": "assistant", "content": resp.content})
        results = []
        for b in resp.content:
            if b.type == "tool_use":
                calls.append((b.name, b.input))
                out, is_err = execute(b.name, b.input)
                results.append({
                    "type": "tool_result",
                    "tool_use_id": b.id,
                    "content": out,
                    "is_error": is_err,
                })
        messages.append({"role": "user", "content": results})
    return calls, "(loop did not terminate)"


WRITE_TOOL = {
    "name": "write_file",
    "description": "Write text content to a file on the user's machine.",
    "input_schema": {
        "type": "object",
        "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
        "required": ["path", "content"],
    },
}

ADD_TOOL = {
    "name": "add",
    "description": "Add two integers and return the sum.",
    "input_schema": {
        "type": "object",
        "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}},
        "required": ["a", "b"],
    },
}

WEATHER_TOOL = {
    "name": "get_weather",
    "description": "Get the current weather for a city.",
    "input_schema": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"],
    },
}

TIME_TOOL = {
    "name": "get_time",
    "description": "Get the current time in a city.",
    "input_schema": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"],
    },
}


def scenario_side_effect():
    # The exact regression: a side-effecting tool must be CALLED, not narrated,
    # and its call must never touch the relay host (we execute it here).
    print("\n[1] Side-effecting tool (write_file) is called, not narrated")
    wrote = {}

    def execute(name, inp):
        wrote[inp["path"]] = inp["content"]
        return "written", False

    calls, _ = run_loop([WRITE_TOOL], "Create a file temp.txt containing exactly: hello", execute)
    check("the model called write_file", any(n == "write_file" for n, _ in calls),
          ", ".join(n for n, _ in calls) or "no tool call — the bug")
    # The model may resolve the path (relative or absolute) — match on basename.
    wrote_temp = next(
        (c for p, c in wrote.items() if os.path.basename(p) == "temp.txt"), None
    )
    check("with the right filename and content",
          wrote_temp is not None and wrote_temp.strip() == "hello", json.dumps(wrote))


def scenario_compute_loop():
    # Full loop: tool_use -> tool_result -> the model uses the result.
    print("\n[2] Compute tool (add) — result flows back into the answer")

    def execute(name, inp):
        return str(int(inp["a"]) + int(inp["b"])), False

    calls, text = run_loop([ADD_TOOL], "What is 21 plus 21? Use the add tool.", execute)
    check("the model called add", any(n == "add" for n, _ in calls))
    check("the final answer contains the sum (42)", "42" in text, repr(text.strip()[:80]))


def scenario_tool_selection():
    # Two similar tools offered — the model must pick the right one.
    print("\n[3] Tool selection — weather asked, weather (not time) called")

    def execute(name, inp):
        return "sunny, 24C" if name == "get_weather" else "15:00", False

    calls, _ = run_loop([WEATHER_TOOL, TIME_TOOL], "What is the weather in Paris?", execute)
    names = [n for n, _ in calls]
    check("get_weather was called", "get_weather" in names, ", ".join(names) or "none")
    check("get_time was not called", "get_time" not in names, ", ".join(names))


def scenario_error_result():
    # A tool that returns is_error — the model must cope, not crash the loop.
    print("\n[4] Tool error result is handled")
    tries = {"n": 0}

    def execute(name, inp):
        tries["n"] += 1
        if tries["n"] == 1:
            return "error: city not found", True
        return "sunny", False

    calls, text = run_loop([WEATHER_TOOL], "Weather in Xyzzy? If it fails, just say you could not find it.", execute)
    check("the model called the tool at least once", len(calls) >= 1, f"{len(calls)} call(s)")
    check("the loop terminated with an answer", text and "did not terminate" not in text,
          repr(text.strip()[:80]))


def main():
    if not TOKEN:
        print("RELAY_TOKEN is required", file=sys.stderr)
        return 2
    for scenario in (
        scenario_side_effect,
        scenario_compute_loop,
        scenario_tool_selection,
        scenario_error_result,
    ):
        try:
            scenario()
        except Exception as exc:  # noqa: BLE001 — a probe: report and keep going
            check(scenario.__name__, False, f"{type(exc).__name__}: {exc}")

    print()
    if failures:
        print(f"FAILED: {len(failures)} check(s): {', '.join(failures)}")
        return 1
    print("All client-tool checks passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
