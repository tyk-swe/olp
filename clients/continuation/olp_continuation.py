"""Official OpenAI Python SDK helper for the versioned OLP tool carrier.

Use the same submission identity for retries of one invocation. The helper
returns tool calls only after the complete durable delivery has been observed.

All three delivery modes — unary, streaming and recovered — construct the same
completed-turn value owned by this helper protocol rather than a concrete SDK
message class:

    version        the negotiated carrier identity
    submission     the submission identity that produced the turn
    handle         the committed continuation handle (ready, recoverable)
    assistant      the exact canonical assistant message representation
    observations   ordered block observations emitted during delivery
    tools          the ordered assistant tool calls (same entries as
                   assistant["tool_calls"], empty when the turn had none)
    finish         the compatible client finish_reason
    usage          the projected Chat usage object
    native_usage   the committed provider-native usage categories
    native_terminal  the committed native terminal record (stop_reason,
                   stop_sequence presence, finish_reason) or None when the
                   delivery predates the record
    actions        the committed explicit actionability claim
                   {"tool_calls": [ordered call ids]}, or None when the
                   delivery predates the claim — a ready handle alone never
                   means "run these tools"
    chunks / response / delivery  the native SDK objects or recorded delivery,
                   kept for inspection only; never canonical next-turn input

Tool argument JSON stays the original string everywhere. Partial or truncated
arguments surface only as observations; they are never completed, invented or
relabeled into an action.
"""

from __future__ import annotations

import json
import time
import uuid
from typing import Any
from urllib.parse import quote
from urllib.request import Request, urlopen

import openai

CONTINUATION_VERSION = "chat-anthropic-tools-v1"

# The carrier admits at most 1024 ordered native blocks, so at most that many
# tool calls and claimed actions can correspond.
_MAX_CALLS = 1024

# Optional SDK message members that sit outside the canonical assistant
# representation. Absent, null or empty values are dropped deliberately; a
# non-empty one is an incompatible delivery, never silently normalized away.
_SDK_MESSAGE_EXTRAS = frozenset({"refusal", "annotations", "audio", "function_call"})
_ASSISTANT_MEMBERS = frozenset({"role", "content", "tool_calls"}) | _SDK_MESSAGE_EXTRAS
_TOOL_CALL_MEMBERS = frozenset({"id", "type", "function"})
_TOOL_FUNCTION_MEMBERS = frozenset({"name", "arguments"})
_ACTION_MEMBERS = frozenset({"tool_calls"})


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
        if not _valid_handle(handle):
            raise ValueError("Invalid continuation handle")
        headers["X-OLP-Continuation-Handle"] = handle
    return headers


def _valid_handle(value: Any) -> bool:
    if not isinstance(value, str) or not value.startswith("continuation_") or len(value) != 45:
        return False
    try:
        uuid.UUID(hex=value.removeprefix("continuation_"))
    except ValueError:
        return False
    return True


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


def _text(value: Any, field: str) -> str:
    if not isinstance(value, str):
        raise ValueError(f"Incomplete continuation delivery ({field})")
    return value


def _tool_call_from(raw: Any) -> dict[str, Any]:
    """Normalize one tool call to the exact carrier shape.

    The argument JSON is kept as its original string — never decoded into
    floating-point values and re-encoded — but it must be a complete native
    object before the call may count as actionable correspondence.
    """
    if not isinstance(raw, dict) or set(raw) - _TOOL_CALL_MEMBERS:
        raise ValueError("Incomplete or unknown assistant tool call")
    if raw.get("type") != "function" or not isinstance(raw.get("id"), str) or not raw["id"]:
        raise ValueError("Unsupported assistant tool call")
    function = raw.get("function")
    if not isinstance(function, dict) or set(function) - _TOOL_FUNCTION_MEMBERS:
        raise ValueError("Incomplete or unknown assistant tool function")
    name, arguments = function.get("name"), function.get("arguments")
    if not isinstance(name, str) or not name or not isinstance(arguments, str) or not arguments:
        raise ValueError("Incomplete assistant tool call")
    try:
        parsed = json.loads(arguments)
    except json.JSONDecodeError:
        # Partial arguments stay observable in chunks/delivery but can never
        # form part of the canonical assistant or an action.
        raise ValueError("Tool call arguments are not a complete JSON value") from None
    if not isinstance(parsed, dict):
        raise ValueError("Tool call arguments are not a native object")
    return {"id": raw["id"], "type": "function", "function": {"name": name, "arguments": arguments}}


def _assistant_from(message: Any) -> dict[str, Any]:
    """Build the exact admitted assistant shape deliberately.

    Only role/content/tool_calls survive; optional SDK properties are dropped
    when empty and rejected when they carry data, so the value corresponds to
    the committed representation instead of whatever the SDK happened to add.
    """
    if not isinstance(message, dict):
        raise ValueError("Incomplete assistant continuation")
    unknown = set(message) - _ASSISTANT_MEMBERS
    if unknown:
        raise ValueError(f"Assistant carries fields outside the carrier: {sorted(unknown)}")
    for extra in _SDK_MESSAGE_EXTRAS:
        if message.get(extra):
            raise ValueError(f"Assistant {extra} is outside the negotiated carrier")
    if message.get("role") != "assistant" or not isinstance(message.get("content"), str):
        raise ValueError("Incomplete assistant continuation")
    assistant: dict[str, Any] = {"role": "assistant", "content": message["content"]}
    calls_raw = message.get("tool_calls")
    if calls_raw is not None:
        if not isinstance(calls_raw, list) or len(calls_raw) > _MAX_CALLS:
            raise ValueError("Incomplete assistant tool calls")
        calls = [_tool_call_from(item) for item in calls_raw]
        if calls:
            assistant["tool_calls"] = calls
    return assistant


def _native_terminal_from(raw: Any) -> dict[str, Any] | None:
    """Validate the committed native terminal record, preserving the
    matched-sequence member exactly: string, explicit null, or absent."""
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise ValueError("Invalid native terminal record in continuation delivery")
    if not isinstance(raw.get("stop_reason"), str) or not isinstance(raw.get("finish_reason"), str):
        raise ValueError("Invalid native terminal record in continuation delivery")
    if "stop_sequence" in raw and raw["stop_sequence"] is not None and not isinstance(raw["stop_sequence"], str):
        raise ValueError("Invalid native stop sequence in continuation delivery")
    terminal: dict[str, Any] = {"stop_reason": raw["stop_reason"]}
    if "stop_sequence" in raw:
        terminal["stop_sequence"] = raw["stop_sequence"]
    terminal["finish_reason"] = raw["finish_reason"]
    return terminal


def _actions_from(raw: Any) -> dict[str, Any] | None:
    """Validate the committed explicit actionability claim.

    The carrier emits {"tool_calls": [ordered ids]} on every ready delivery it
    commits under this contract revision. An absent member — or the recovery
    endpoint's literal "unavailable" marker for older committed deliveries —
    produces None: the turn stays observable and recoverable but exposes no
    action. Unknown claim members are incompatible and rejected.
    """
    if raw is None or raw == "unavailable":
        return None
    if not isinstance(raw, dict) or set(raw) - _ACTION_MEMBERS:
        raise ValueError("Invalid continuation actions in delivery")
    claimed = raw.get("tool_calls")
    if (
        not isinstance(claimed, list)
        or len(claimed) > _MAX_CALLS
        or any(not isinstance(identifier, str) or not identifier for identifier in claimed)
    ):
        raise ValueError("Invalid continuation actions in delivery")
    return {"tool_calls": list(claimed)}


def _check_action_correspondence(actions: dict[str, Any] | None, calls: list[dict[str, Any]]) -> None:
    """The committed claim must name exactly the ordered assistant calls.
    A claim that adds, drops or reorders identities is an incomplete
    correspondence and the whole delivery is rejected before any action is
    exposed."""
    if actions is None:
        return
    if actions["tool_calls"] != [call["id"] for call in calls]:
        raise ValueError("Continuation actions do not correspond to the assistant tool calls")


def _check_terminal(terminal: dict[str, Any] | None, finish: str) -> None:
    # The committed record's compatible finish reason must equal the delivered
    # finish reason; a contradiction is a corrupt delivery.
    if terminal is not None and terminal["finish_reason"] != finish:
        raise ValueError("Native terminal record does not match the delivered finish")


def _completed(
    submission: str,
    handle: str,
    assistant: dict[str, Any],
    observations: list[Any],
    finish: str,
    usage: Any,
    native_usage: Any,
    native_terminal: dict[str, Any] | None,
    actions: dict[str, Any] | None,
) -> dict[str, Any]:
    tools = list(assistant.get("tool_calls") or [])
    return {
        "version": CONTINUATION_VERSION,
        "submission": submission,
        "handle": handle,
        "assistant": assistant,
        "observations": observations,
        "tools": tools,
        "finish": finish,
        "usage": usage,
        "native_usage": native_usage,
        "native_terminal": native_terminal,
        "actions": actions,
    }


def _assemble(frames: list[Any], submission: str) -> dict[str, Any]:
    """Fold ordered Chat chunk objects into the one completed-turn value.

    Used by live streaming delivery (SDK chunks serialized to plain dicts) and
    by recovery (the committed recorded frames) so both construct exactly the
    same value.
    """
    observations: list[Any] = []
    calls: dict[int, dict[str, Any]] = {}
    text = ""
    ready_handle = finish = usage = native_usage = None
    native_terminal = None
    actions = None
    terminal = False
    for raw in frames:
        if not isinstance(raw, dict):
            raise ValueError("Malformed continuation delivery frame")
        ext = raw.get("olp")
        if not isinstance(ext, dict) or ext.get("version") != CONTINUATION_VERSION:
            raise ValueError("The SDK did not retain the negotiated OLP continuation extension")
        choices = raw.get("choices")
        if not isinstance(choices, list) or len(choices) > 1:
            raise ValueError("Unsupported continuation candidate count")
        if "observation" in ext:
            if terminal:
                raise ValueError("Observation followed terminal delivery")
            observations.append(ext["observation"])
        choice = choices[0] if choices else None
        if choice is not None and not isinstance(choice, dict):
            raise ValueError("Malformed continuation delivery choice")
        if choice:
            delta = choice.get("delta")
            if delta is None:
                delta = {}
            if not isinstance(delta, dict):
                raise ValueError("Malformed continuation delivery delta")
            if isinstance(delta.get("content"), str):
                text += delta["content"]
            for call in delta.get("tool_calls") or []:
                if not isinstance(call, dict) or not isinstance(call.get("index"), int) or call["index"] < 0:
                    raise ValueError("Invalid tool index")
                prior = calls.setdefault(
                    call["index"], {"id": "", "type": "function", "function": {"name": "", "arguments": ""}}
                )
                if call.get("id"):
                    prior["id"] += call["id"]
                if call.get("type") and call["type"] != "function":
                    raise ValueError("Unsupported tool type")
                function = call.get("function") or {}
                if function.get("name"):
                    prior["function"]["name"] += function["name"]
                if function.get("arguments"):
                    prior["function"]["arguments"] += function["arguments"]
            finish_reason = choice.get("finish_reason")
            if finish_reason is not None:
                if terminal or ext.get("ready") is not True or not isinstance(ext.get("handle"), str):
                    raise ValueError("Terminal delivery has no committed continuation")
                finish = _text(finish_reason, "finish_reason")
                ready_handle = ext["handle"]
                usage = raw.get("usage")
                native_usage = ext.get("native_usage")
                native_terminal = _native_terminal_from(ext.get("native_terminal"))
                _check_terminal(native_terminal, finish)
                actions = _actions_from(ext.get("actions"))
                terminal = True
    if not terminal or not _valid_handle(ready_handle) or not native_usage:
        raise ValueError("Incomplete continuation delivery")
    ordered = []
    for index, call in sorted(calls.items()):
        if index != len(ordered) or not call["id"] or not call["function"]["name"] or not call["function"]["arguments"]:
            raise ValueError("Incomplete or reordered tool call")
        # Streamed calls assemble only from complete argument strings; the
        # same complete-object validation as unary applies before the call may
        # correspond to an action.
        _tool_call_from(call)
        ordered.append(call)
    if (finish == "tool_calls") != bool(ordered):
        raise ValueError("Tool terminal mismatch")
    _check_action_correspondence(actions, ordered)
    assistant: dict[str, Any] = {"role": "assistant", "content": text}
    if ordered:
        assistant["tool_calls"] = ordered
    return _completed(
        submission, ready_handle, assistant, observations, finish, usage, native_usage, native_terminal, actions
    )


def _completed_unary(body: Any, submission: str) -> dict[str, Any]:
    """Fold one Chat completion body — live SDK response or committed recorded
    body — into the same completed-turn value a streamed turn produces."""
    if not isinstance(body, dict):
        raise ValueError("Malformed continuation delivery body")
    ext = body.get("olp")
    if (
        not isinstance(ext, dict)
        or ext.get("version") != CONTINUATION_VERSION
        or ext.get("ready") is not True
        or not isinstance(ext.get("handle"), str)
        or ext.get("native_usage") is None
    ):
        raise ValueError("Incomplete continuation delivery")
    choices = body.get("choices")
    if not isinstance(choices, list) or len(choices) != 1 or not isinstance(choices[0], dict):
        raise ValueError("The result has an unsupported candidate count")
    choice = choices[0]
    finish = _text(choice.get("finish_reason"), "finish_reason")
    assistant = _assistant_from(choice.get("message"))
    observations = ext.get("observations")
    if observations is None:
        observations = []
    if not isinstance(observations, list):
        raise ValueError("Invalid continuation observations")
    native_terminal = _native_terminal_from(ext.get("native_terminal"))
    _check_terminal(native_terminal, finish)
    actions = _actions_from(ext.get("actions"))
    ordered = list(assistant.get("tool_calls") or [])
    if (finish == "tool_calls") != bool(ordered):
        raise ValueError("Tool terminal mismatch")
    _check_action_correspondence(actions, ordered)
    return _completed(
        submission,
        ext["handle"],
        assistant,
        observations,
        finish,
        body.get("usage"),
        ext["native_usage"],
        native_terminal,
        actions,
    )


def _sdk_json(value: Any) -> dict[str, Any]:
    """Serialize an SDK object back to its plain JSON object so the canonical
    value never depends on a concrete SDK message class. Tool argument strings
    survive untouched; the negotiated extension is reattached explicitly."""
    raw = json.loads(value.model_dump_json())
    if not isinstance(raw, dict):
        raise ValueError("Malformed continuation delivery")
    return raw


def stream_turn(
    client: Any, request: dict[str, Any], submission: str | None = None, handle: str | None = None
) -> dict[str, Any]:
    _check_sdk(client)
    submission = submission or submission_id()
    stream = client.chat.completions.create(
        **{**request, "stream": True}, extra_headers=continuation_headers(submission, handle)
    )
    chunks: list[Any] = []
    frames: list[dict[str, Any]] = []
    for chunk in stream:
        ext = _extension(chunk)
        chunks.append(chunk)
        raw = _sdk_json(chunk)
        raw["olp"] = ext
        frames.append(raw)
    completed = _assemble(frames, submission)
    completed["chunks"] = chunks
    return completed


def next_turn(request: dict[str, Any], completed: dict[str, Any], results: list[dict[str, str]]) -> dict[str, Any]:
    """Build the next request for a completed turn.

    The completed value must be the one this helper produced: the negotiated
    carrier version, a valid committed handle and an explicit actions claim
    that corresponds exactly to the assistant's ordered tool calls. A ready
    handle without a tool action — a partial or non-tool outcome, or a
    delivery recorded before the claim existed — can be inspected and
    recovered but never yields executable calls here.
    """
    if not isinstance(completed, dict) or completed.get("version") != CONTINUATION_VERSION:
        raise ValueError("Provide the completed turn value produced by this helper")
    if not _valid_handle(completed.get("handle")):
        raise ValueError("The completed turn has no committed continuation handle")
    assistant = _assistant_from(completed.get("assistant"))
    actions = _actions_from(completed.get("actions"))
    if actions is None:
        raise ValueError("The completed turn has no actionability claim")
    _check_action_correspondence(actions, assistant.get("tool_calls") or [])
    calls = actions["tool_calls"]
    if not calls:
        raise ValueError("The completed turn exposes no tool actions")
    ordered = assistant.get("tool_calls") or []
    if not isinstance(results, list) or len(results) != len(calls):
        raise ValueError("Provide one result for each ready tool call")
    tools = []
    for call, result in zip(ordered, results, strict=True):
        if not isinstance(result, dict) or result.get("tool_call_id") != call["id"] or not isinstance(result.get("content"), str):
            raise ValueError("Tool results must match call order and identity")
        tools.append({"role": "tool", "tool_call_id": call["id"], "content": result["content"]})
    next_request = {
        **request,
        "messages": [*request["messages"], assistant, *tools],
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
    if ext.get("ready") is not True or not isinstance(ext.get("handle"), str) or not ext.get("native_usage"):
        raise ValueError("Incomplete continuation delivery")
    body = _sdk_json(response)
    body["olp"] = ext
    completed = _completed_unary(body, submission)
    completed["response"] = response
    return completed


def recover_submission(origin: str, api_key: str, submission: str) -> dict[str, Any]:
    """Read a committed delivery without asking the provider to work again.

    The recorded unary body or streaming frames are reassembled through the
    same construction as a live turn and cross-checked against the committed
    assistant, handle, native terminal record and action claim. A delivery
    committed before the outcome contract existed reads those members as
    "unavailable" and exposes no tool action on replay — the helper never
    upgrades a stored outcome.
    """
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
        not isinstance(state, dict)
        or state.get("version") != CONTINUATION_VERSION
        or state.get("state") != "ready"
        or not _valid_handle(state.get("handle"))
        or not state.get("assistant")
        or not isinstance(state.get("delivery"), dict)
    ):
        raise ValueError("Incomplete recoverable continuation delivery")
    delivery = state["delivery"]
    if delivery.get("stream") is True and isinstance(delivery.get("frames"), list):
        completed = _assemble(delivery["frames"], submission)
    elif delivery.get("stream") is False and isinstance(delivery.get("body"), dict):
        completed = _completed_unary(delivery["body"], submission)
    else:
        raise ValueError("This submission has another delivery shape")
    if _assistant_from(state["assistant"]) != completed["assistant"]:
        raise ValueError("Recovered assistant does not match the committed delivery")
    if completed["handle"] != state["handle"]:
        raise ValueError("Recovered handle does not match the committed delivery")
    committed_terminal = state.get("native_terminal")
    if committed_terminal == "unavailable":
        if completed["native_terminal"] is not None:
            raise ValueError("Recovered terminal does not match the committed delivery")
    elif committed_terminal is not None and _native_terminal_from(committed_terminal) != completed["native_terminal"]:
        raise ValueError("Recovered terminal does not match the committed delivery")
    committed_actions = state.get("actions")
    if committed_actions == "unavailable":
        if completed["actions"] is not None:
            raise ValueError("Recovered actions do not match the committed delivery")
    elif committed_actions is not None and _actions_from(committed_actions) != completed["actions"]:
        raise ValueError("Recovered actions do not match the committed delivery")
    completed["delivery"] = delivery
    return completed
