from __future__ import annotations

import os
import sys
from pathlib import Path
from urllib.parse import urlsplit

import openai

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "clients/continuation"))
from olp_continuation import next_turn, stream_turn, unary_turn  # noqa: E402

origin = os.environ["OLP_CONTINUATION_ORIGIN"]
key = os.environ["OLP_CONTINUATION_KEY"]
route = os.environ["OLP_CONTINUATION_ROUTE"]
parsed = urlsplit(origin)
assert parsed.scheme == "http" and parsed.hostname == "127.0.0.1" and parsed.port
assert key.startswith("olp_") and route.startswith("strict-")

class UnsupportedClient:
    def __init__(self) -> None:
        self.calls = 0

    @property
    def chat(self) -> "UnsupportedClient":
        self.calls += 1
        return self

unsupported = UnsupportedClient()
try:
    stream_turn(unsupported, {"model": route, "messages": []})
    raise AssertionError("unsupported SDK unexpectedly dispatched")
except ValueError as error:
    assert "supports the qualified" in str(error)
assert unsupported.calls == 0

request = {
    "model": route,
    "stream": True,
    "tools": [
        {"type": "function", "function": {"name": "weather", "description": "Weather in a city", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}},
        {"type": "function", "function": {"name": "clock", "description": "Time in a zone", "parameters": {"type": "object", "properties": {"zone": {"type": "string"}}, "required": ["zone"]}}},
    ],
    "messages": [{"role": "user", "content": "Weather and time in Paris?"}],
}
with openai.OpenAI(api_key=key, base_url=f"{origin}/v1", max_retries=0, timeout=15.0) as client:
    try:
        client.chat.completions.create(**request)
        raise AssertionError("unnegotiated SDK unexpectedly dispatched")
    except openai.BadRequestError as error:
        assert error.status_code == 400
        assert error.code == "state_carrier"

    first = stream_turn(client, request)
    assert first["finish"] == "tool_calls"
    assert first["assistant"]["content"] == "beforeafter"
    assert [call["id"] for call in first["assistant"]["tool_calls"]] == ["call-weather", "call-clock"]
    assert [item["type"] for item in first["observations"] if item["phase"] == "start"] == [
        "thinking", "text", "tool_use", "tool_use", "text"
    ]
    assert any(item["type"] == "thinking" and item.get("opaque_state") is True for item in first["observations"])
    assert "opaque-fixture-signature-do-not-log" not in repr(first["chunks"])
    replay = stream_turn(client, request, submission=first["submission"])
    assert replay["handle"] == first["handle"]
    assert replay["assistant"] == first["assistant"]
    next_request = next_turn(request, first, [
        {"tool_call_id": "call-weather", "content": "sunny"},
        {"tool_call_id": "call-clock", "content": "14:00"},
    ])
    final = unary_turn(client, next_request, handle=first["handle"])
    assert final["response"].choices[0].message.content == "Both tools completed."
    assert final["response"].choices[0].finish_reason == "stop"
    print(f"openai-python-3.8.0: {len(first['observations'])} observations; two native dispatches")
