from __future__ import annotations

import http.client
import json
import os
import sys
from collections.abc import Callable
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

if "--check-metadata" in sys.argv:
    raise SystemExit(0)

import anthropic
import openai
from google import genai
from google.genai import errors, types

openai_base_urls = (
    ("canonical OpenAI base", f"{origin}/openai/v1"),
    ("canonical OpenAI base with trailing slash", f"{origin}/openai/v1/"),
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


def smoke_openai_litellm() -> None:
    with openai_client(
        f"{origin}/v1/",
        client_api_key="external-upstream-authorization",
        default_headers={"x-litellm-api-key": api_key},
    ) as client:
        raw_response = client.models.with_raw_response.list()
        page = raw_response.parse()
        assert any(model.id == route_slug for model in page.data)
        headers = raw_response.http_response.request.headers
        assert headers.get("authorization") == "Bearer external-upstream-authorization"
        assert headers.get("x-litellm-api-key") == api_key


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
    cases = (
        (
            "an invalid x-litellm-api-key",
            {"x-litellm-api-key": invalid_api_key},
        ),
        (
            "an invalid x-litellm-api-key must not fall back to a valid native key",
            {
                "x-litellm-api-key": invalid_api_key,
                "Authorization": f"Bearer {api_key}",
            },
        ),
        (
            "conflicting valid OLP gateway credentials",
            {
                "x-litellm-api-key": api_key,
                "Authorization": f"Bearer {conflict_api_key}",
            },
        ),
    )
    for description, headers in cases:
        assert direct_status("/v1/models", headers) == 401, description
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
    smoke_openai_litellm()
    smoke_anthropic()
    smoke_google()
    for label, base_url in openai_base_urls:
        error_contract_openai(base_url, label)
    direct_negative_contracts()
    error_contract_anthropic()
    error_contract_google()
    print("Official Python OpenAI, Anthropic, and Google GenAI SDK contracts passed.")


if __name__ == "__main__":
    main()
