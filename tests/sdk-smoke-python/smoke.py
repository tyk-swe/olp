from __future__ import annotations

import http.client
import json
import os
import sys
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from urllib.parse import urlsplit

if not __debug__:
    raise RuntimeError("SDK smoke contract checks require an unoptimized Python interpreter")


def load_metadata() -> dict[str, str]:
    metadata_path = os.environ.get("OLP_SDK_SMOKE_METADATA")
    assert metadata_path, "OLP_SDK_SMOKE_METADATA is required"
    with open(metadata_path, encoding="utf-8") as metadata_file:
        metadata = json.load(metadata_file)
    assert isinstance(metadata, dict)
    return metadata


metadata = load_metadata()
origin = metadata["origin"]
api_key = metadata["api_key"]
conflict_api_key = metadata["conflict_api_key"]
route_slug = metadata["route_slug"]
native_tool_route = metadata["native_tool_route"]
verification_origin = urlsplit(metadata["verification_origin"])
origin_url = urlsplit(origin)
invalid_api_key = "olp_not-a-real-key"

assert origin_url.scheme == "http"
assert origin_url.hostname == "127.0.0.1"
assert origin_url.port is not None
assert origin == f"http://127.0.0.1:{origin_url.port}"
assert route_slug == "sdk-smoke-route"
assert api_key.startswith("olp_")
assert conflict_api_key.startswith("olp_")
assert conflict_api_key != api_key
assert native_tool_route == "sdk-native-tools-route"
assert verification_origin.scheme == "http"
assert verification_origin.hostname == "127.0.0.1"
assert verification_origin.port is not None
assert verification_origin.netloc != origin_url.netloc

if "--check-metadata" in sys.argv:
    raise SystemExit(0)

import anthropic
import openai
from google import genai
from google.genai import errors, types

openai_base_urls = (
    ("canonical OpenAI base", f"{origin}/v1"),
    ("canonical OpenAI base with trailing slash", f"{origin}/v1/"),
    ("OpenAI compatibility base", f"{origin}/v1"),
    ("OpenAI compatibility base with trailing slash", f"{origin}/v1/"),
)


def openai_client(
    base_url: str,
    client_api_key: str = api_key,
    default_headers: dict[str, str] | None = None,
) -> openai.OpenAI:
    return openai.OpenAI(
        api_key=client_api_key,
        base_url=base_url,
        default_headers=default_headers,
        max_retries=0,
        timeout=5.0,
    )


def anthropic_client(client_api_key: str = api_key) -> anthropic.Anthropic:
    return anthropic.Anthropic(
        api_key=client_api_key,
        base_url=f"{origin}/anthropic",
        max_retries=0,
        timeout=5.0,
    )


def google_client(client_api_key: str = api_key) -> genai.Client:
    return genai.Client(
        vertexai=False,
        api_key=client_api_key,
        http_options=types.HttpOptions(
            base_url=f"{origin}/gemini",
            api_version="v1beta",
            timeout=5_000,
            retry_options=types.HttpRetryOptions(attempts=1),
        ),
    )


def smoke_openai(base_url: str, label: str) -> None:
    with openai_client(base_url) as client:
        completion = client.chat.completions.create(
            model=route_slug,
            max_tokens=32,
            messages=[{"role": "user", "content": "official SDK smoke"}],
        )
        assert completion.model == route_slug
        assert completion.choices[0].message.content == (
            f"official openai sdk reached {route_slug}"
        )

        response = client.responses.create(
            model=route_slug,
            input="official Responses SDK smoke",
        )
        assert response.output_text == f"official openai sdk reached {route_slug}"

        streaming = client.chat.completions.create(
            model=route_slug,
            max_tokens=32,
            stream=True,
            messages=[{"role": "user", "content": "official streaming SDK smoke"}],
        )
        streamed_text = "".join(
            chunk.choices[0].delta.content or ""
            for chunk in streaming
            if chunk.choices
        )
        assert streamed_text == f"official openai sdk reached {route_slug}"

        page = client.models.list()
        assert any(model.id == route_slug for model in page.data), label
        assert client.models.retrieve(route_slug).id == route_slug, label


def smoke_anthropic() -> None:
    with anthropic_client() as client:
        message = client.messages.create(
            model=route_slug,
            max_tokens=32,
            messages=[{"role": "user", "content": "official SDK smoke"}],
        )
        assert message.model == route_slug
        assert message.content[0].type == "text"
        assert message.content[0].text == f"official anthropic sdk reached {route_slug}"

        with client.messages.stream(
            model=route_slug,
            max_tokens=32,
            messages=[{"role": "user", "content": "official streaming SDK smoke"}],
        ) as stream:
            streamed = stream.get_final_message()
        assert streamed.content[0].type == "text"
        assert streamed.content[0].text == f"official anthropic sdk reached {route_slug}"

        page = client.models.list(limit=10)
        assert any(model.id == route_slug for model in page.data)
        count = client.messages.count_tokens(
            model=route_slug,
            messages=[{"role": "user", "content": "official token count SDK smoke"}],
        )
        assert count.input_tokens == 13


def native_anthropic_tool_workflow() -> None:
    reference_path = (
        Path(__file__).resolve().parents[1]
        / "fixtures/fidelity/v1/anthropic-tool-next-request.json"
    )
    reference = json.loads(reference_path.read_text(encoding="utf-8"))
    assert reference["model"] == "fixture-model"
    expected_messages = reference["messages"]
    controls = {
        key: value for key, value in reference.items()
        if key not in ("model", "messages")
    }

    def counts() -> dict[str, object]:
        # Fixture instrumentation uses its own listener, never an OLP endpoint.
        connection = http.client.HTTPConnection(
            "127.0.0.1", verification_origin.port, timeout=5
        )
        try:
            connection.request("GET", "/native-tool-workflow")
            response = connection.getresponse()
            assert response.status == 200
            return json.loads(response.read())
        finally:
            connection.close()

    assert counts() == {
        "dispatches": 0, "initial_requests": 0, "next_requests": 0,
        "rejected_requests": 0, "complete": False,
    }
    with anthropic_client() as client:
        history = [expected_messages[0]]
        with client.messages.stream(
            model=native_tool_route, messages=history, **controls
        ) as stream:
            events = list(stream)
            assistant = stream.get_final_message()
        # Python's helper also yields convenience text/thinking/input events.
        # Count the native event types, retaining the SDK's own assembly path.
        native_types = {
            "message_start", "message_delta", "message_stop",
            "content_block_start", "content_block_delta", "content_block_stop",
        }
        native_events = [event for event in events if event.type in native_types]
        assert len(native_events) == 19, "all frozen native events must reach the SDK"
        assert events[-1].type == "message_stop"
        assert [
            event.index for event in events if event.type == "content_block_start"
        ] == [0, 1, 2, 3, 4]
        assert assistant.model == native_tool_route
        assert assistant.stop_reason == "tool_use"
        assert assistant.usage.input_tokens == 18
        assert assistant.usage.output_tokens == 28
        assert [block.to_dict() for block in assistant.content] == expected_messages[1]["content"], (
            "SDK assembly must retain thinking/signature and text/tool/text order"
        )
        calls = [block for block in assistant.content if block.type == "tool_use"]
        assert [call.id for call in calls] == ["call-weather", "call-clock"]

        def execute(call: anthropic.types.ToolUseBlock) -> dict[str, str]:
            if call.name == "weather":
                assert call.input == {"city": "Paris"}
                content = "sunny"
            elif call.name == "clock":
                assert call.input == {"zone": "Europe/Paris"}
                content = "14:00"
            else:
                raise AssertionError("unexpected native tool")
            return {"type": "tool_result", "tool_use_id": call.id, "content": content}

        with ThreadPoolExecutor(max_workers=2) as executor:
            results = list(executor.map(execute, calls))
        assert len(results) == 2
        assert len({result["tool_use_id"] for result in results}) == 2
        # Feed actual SDK content objects into the SDK serializer. The scripted
        # provider independently checks every native next-request dependency.
        final = client.messages.create(
            model=native_tool_route,
            messages=[
                *history,
                {"role": assistant.role, "content": assistant.content},
                {"role": "user", "content": results},
            ],
            **controls,
        )
        assert final.id == "msg-native-tool-final"
        assert final.model == native_tool_route
        assert final.stop_reason == "end_turn"
        assert [block.to_dict() for block in final.content] == [
            {"type": "text", "text": "Weather: sunny. Time: 14:00."}
        ]
        assert final.usage.input_tokens == 64
        assert final.usage.output_tokens == 8
    assert counts() == {
        "dispatches": 2, "initial_requests": 1, "next_requests": 1,
        "rejected_requests": 0, "complete": True,
    }
    print(
        "Native Anthropic Python SDK reasoning/tool continuation passed: "
        "19 events, 2 tools, 2 verified dispatches."
    )


def smoke_google() -> None:
    with google_client() as client:
        response = client.models.generate_content(
            model=route_slug,
            contents="official SDK smoke",
        )
        assert response.text == f"official gemini sdk reached {route_slug}"
        assert response.model_version == route_slug

        streaming = client.models.generate_content_stream(
            model=route_slug,
            contents="official streaming SDK smoke",
        )
        streamed_text = "".join(chunk.text or "" for chunk in streaming)
        assert streamed_text == f"official gemini sdk reached {route_slug}"

        pager = client.models.list(config=types.ListModelsConfig(page_size=10))
        assert f"models/{route_slug}" in {model.name for model in pager}


def rejected(attempt: Callable[[], object], description: str) -> Exception:
    try:
        attempt()
    except Exception as error:
        return error
    raise AssertionError(f"{description} was expected to fail but succeeded")


def error_contract_openai(base_url: str, label: str) -> None:
    with openai_client(base_url, invalid_api_key) as wrong_key:
        unauthorized = rejected(
            lambda: wrong_key.chat.completions.create(
                model=route_slug,
                max_tokens=32,
                messages=[{"role": "user", "content": "invalid credential"}],
            ),
            f"{label} with an invalid key",
        )
    assert isinstance(unauthorized, openai.AuthenticationError)
    assert unauthorized.status_code == 401

    with openai_client(base_url) as client:
        missing = rejected(
            lambda: client.chat.completions.create(
                model="sdk-smoke-no-such-route",
                max_tokens=32,
                messages=[{"role": "user", "content": "unknown model"}],
            ),
            f"{label} with an unknown model",
        )
    assert isinstance(missing, openai.NotFoundError)
    assert missing.status_code == 404


def direct_status(path: str, headers: dict[str, str]) -> int:
    connection = http.client.HTTPConnection("127.0.0.1", origin_url.port, timeout=5)
    try:
        connection.request("GET", path, headers=headers)
        response = connection.getresponse()
        response.read()
        return response.status
    finally:
        connection.close()


def direct_negative_contracts() -> None:
    assert direct_status("/v1/models", {"x-litellm-api-key": api_key}) == 401
    assert direct_status(
        "/openai/v1/models", {"Authorization": f"Bearer {api_key}"}
    ) == 404
    assert direct_status(
        "/v1/not-enabled",
        {"Authorization": f"Bearer {api_key}"},
    ) == 404


def error_contract_anthropic() -> None:
    with anthropic_client(invalid_api_key) as wrong_key:
        unauthorized = rejected(
            lambda: wrong_key.messages.create(
                model=route_slug,
                max_tokens=32,
                messages=[{"role": "user", "content": "invalid credential"}],
            ),
            "an Anthropic call with an invalid key",
        )
    assert isinstance(unauthorized, anthropic.AuthenticationError)
    assert unauthorized.status_code == 401
    assert unauthorized.type == "authentication_error"


def error_contract_google() -> None:
    with google_client(invalid_api_key) as wrong_key:
        unauthorized = rejected(
            lambda: wrong_key.models.generate_content(
                model=route_slug,
                contents="invalid credential",
            ),
            "a Gemini call with an invalid key",
        )
    assert isinstance(unauthorized, errors.ClientError)
    assert unauthorized.code == 401


def main() -> None:
    for label, base_url in openai_base_urls:
        smoke_openai(base_url, label)
    smoke_anthropic()
    native_anthropic_tool_workflow()
    smoke_google()
    for label, base_url in openai_base_urls:
        error_contract_openai(base_url, label)
    direct_negative_contracts()
    error_contract_anthropic()
    error_contract_google()
    print("Official Python OpenAI, Anthropic, and Google GenAI SDK contracts passed.")


if __name__ == "__main__":
    main()
