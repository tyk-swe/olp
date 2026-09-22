"""Official OpenAI Python SDK helper for the versioned OLP tool carrier.

Use the same submission identity for retries of one invocation. The helper
returns tool calls only after the complete durable delivery has been observed.
"""

from __future__ import annotations

import time
import uuid
import json
from typing import Any
from urllib.request import Request, urlopen
from urllib.parse import quote

import openai

CONTINUATION_VERSION = "chat-anthropic-tools-v1"


def submission_id() -> str:
    return f"{int(time.time() * 1000)}.{uuid.uuid4()}"


def continuation_headers(submission: str, handle: str | None = None) -> dict[str, str]:
    stamp, sep, unique = submission.partition(".")
    if not sep or not stamp.isdecimal() or not unique or str(uuid.UUID(unique)) != unique:
        raise ValueError("Use a timestamp.UUID submission identity")
    headers = {
        "X-OLP-Continuation": CONTINUATION_VERSION,
        "X-OLP-Submission-ID": submission,
    }
    if handle is not None:
        if not handle.startswith("continuation_") or len(handle) != 45:
            raise ValueError("Invalid continuation handle")
        uuid.UUID(hex=handle.removeprefix("continuation_"))
        headers["X-OLP-Continuation-Handle"] = handle
    return headers


def _extension(value: Any) -> dict[str, Any]:
    extension = getattr(value, "olp", None)
    if extension is None:
        extension = value.model_extra.get("olp") if value.model_extra else None
    if not isinstance(extension, dict) or extension.get("version") != CONTINUATION_VERSION:
        raise ValueError("The SDK did not retain the negotiated OLP continuation extension")
    return extension


def _check_sdk(client: Any) -> None:
    if openai.__version__ != "3.8.0" or not isinstance(client, openai.OpenAI):
        raise ValueError("This helper supports the qualified OpenAI Python SDK 3.8.0 only")


def stream_turn(
    client: Any, request: dict[str, Any], submission: str | None = None, handle: str | None = None
) -> dict[str, Any]:
    _check_sdk(client)
    submission = submission or submission_id()
    stream = client.chat.completions.create(
        **{**request, "stream": True}, extra_headers=continuation_headers(submission, handle)
    )
    observations: list[dict[str, Any]] = []
    calls: dict[int, dict[str, Any]] = {}
    chunks: list[Any] = []
    text = ""
    ready_handle = None
    finish = None
    usage = None
    terminal = False
    for chunk in stream:
        ext = _extension(chunk)
        chunks.append(chunk)
        choice = chunk.choices[0] if chunk.choices else None
        if "observation" in ext:
            if terminal:
                raise ValueError("Observation followed terminal delivery")
            observations.append(ext["observation"])
        if choice and choice.delta.content:
            text += choice.delta.content
        for call in choice.delta.tool_calls or [] if choice else []:
            if call.index is None or call.index < 0:
                raise ValueError("Invalid tool index")
            prior = calls.setdefault(call.index, {
                "id": "", "type": "function", "function": {"name": "", "arguments": ""}
            })
            if call.id:
                prior["id"] += call.id
            if call.type and call.type != "function":
                raise ValueError("Unsupported tool type")
            if call.function and call.function.name:
                prior["function"]["name"] += call.function.name
            if call.function and call.function.arguments:
                prior["function"]["arguments"] += call.function.arguments
        if choice and choice.finish_reason:
            if terminal or ext.get("ready") is not True or not isinstance(ext.get("handle"), str):
                raise ValueError("Terminal delivery has no committed continuation")
            finish = choice.finish_reason
            ready_handle = ext["handle"]
            usage = chunk.usage
            terminal = True
    if not terminal or not ready_handle:
        raise ValueError("Incomplete continuation delivery")
    ordered = []
    for index, call in sorted(calls.items()):
        if index != len(ordered) or not call["id"] or not call["function"]["name"] or not call["function"]["arguments"]:
            raise ValueError("Incomplete or reordered tool call")
        ordered.append(call)
    if (finish == "tool_calls") != bool(ordered):
        raise ValueError("Tool terminal mismatch")
    assistant = {"role": "assistant", "content": text}
    if ordered:
        assistant["tool_calls"] = ordered
    return {
        "submission": submission, "handle": ready_handle, "assistant": assistant,
        "observations": observations, "chunks": chunks, "finish": finish, "usage": usage,
    }


def next_turn(request: dict[str, Any], completed: dict[str, Any], results: list[dict[str, str]]) -> dict[str, Any]:
    calls = completed["assistant"].get("tool_calls")
    if not completed.get("handle") or not isinstance(calls, list) or len(results) != len(calls):
        raise ValueError("Provide one result for each ready tool call")
    tools = []
    for call, result in zip(calls, results, strict=True):
        if result.get("tool_call_id") != call["id"] or not isinstance(result.get("content"), str):
            raise ValueError("Tool results must match call order and identity")
        tools.append({"role": "tool", "tool_call_id": call["id"], "content": result["content"]})
    next_request = {
        **request,
        "messages": [*request["messages"], completed["assistant"], *tools],
    }
    next_request.pop("stream", None)
    next_request.pop("stream_options", None)
    return next_request


def unary_turn(
    client: Any, request: dict[str, Any], submission: str | None = None, handle: str | None = None
) -> dict[str, Any]:
    _check_sdk(client)
    submission = submission or submission_id()
    response = client.chat.completions.create(
        **request, extra_headers=continuation_headers(submission, handle)
    )
    ext = _extension(response)
    if ext.get("ready") is not True or not ext.get("handle") or not response.choices[0].message:
        raise ValueError("Incomplete continuation delivery")
    return {
        "submission": submission, "handle": ext["handle"],
        "assistant": response.choices[0].message, "response": response,
    }


def recover_submission(origin: str, api_key: str, submission: str) -> dict[str, Any]:
    """Read a committed delivery without asking the provider to work again."""
    continuation_headers(submission)
    request = Request(
        f"{origin.rstrip('/')}/v1/continuation-submissions/{quote(submission, safe='')}",
        headers={
            "Authorization": f"Bearer {api_key}",
            "X-OLP-Continuation": CONTINUATION_VERSION,
        },
    )
    with urlopen(request, timeout=15) as response:
        state = json.load(response)
    if (
        state.get("version") != CONTINUATION_VERSION
        or state.get("state") != "ready"
        or not state.get("handle", "").startswith("continuation_")
        or not state.get("assistant")
        or not state.get("delivery")
    ):
        raise ValueError("Incomplete recoverable continuation delivery")
    return state
