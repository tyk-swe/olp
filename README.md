# OpenLLMProxy

[![CI](https://github.com/tyk-swe/olp/actions/workflows/ci.yml/badge.svg)](https://github.com/tyk-swe/olp/actions/workflows/ci.yml)
[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/tyk-swe/olp)](https://github.com/tyk-swe/olp/releases)

OpenLLMProxy is a self-hosted AI gateway and control plane. It presents
OpenAI-, Anthropic-, and Gemini-compatible APIs and routes stable public route
slugs to OpenAI, Anthropic, Gemini, Vertex AI, Azure OpenAI, Amazon Bedrock,
and certified OpenAI-compatible providers.

![OpenLLMProxy console — overview dashboard](docs/assets/screenshots/overview.png)

## Features

- Stable protocol surfaces with explicit route and provider policy; direct
  provider/model addressing is intentionally unavailable.
- Priority groups, weighted selection, bounded failover, and pre-activation
  simulation over certified provider/model/operation tuples.
- Immutable, digested runtime generations: a stream keeps one generation and
  credential revision for its entire lifetime.
- Attempt-level usage and cost accounting with operational metadata only —
  never prompts, outputs, tool data, uploads, or raw credentials.
- Installation-scoped keys, route permissions, expiry, hard distributed rate,
  concurrency, daily cost, and monthly cost limits, plus an audit stream for
  administrative changes. Unpriced attempts accrue 0 and are counted
  separately instead of being assigned an invented cost.
- Private health/metrics, backup/restore, upgrade rehearsal, and HA workers.

## Quick start

Prerequisites: Docker with Compose support and OpenSSL.

The release-image quick start requires public `v2.3.0` artifacts. For a
pre-release checkout or source changes, use the contributor build overlay
described below.

```bash
cp .env.example .env
./scripts/prepare-compose-secrets.sh
docker compose --env-file .env \
  -f deploy/compose.yaml -f deploy/compose.bootstrap.yaml up -d
```

Contributors building from source should add `-f deploy/compose.build.yaml`
beside the other Compose files and add `--build` to `up`. Keep the build
overlay and `--build` on the post-bootstrap recreation command as well.

The helper creates only missing secrets, including the one-time bootstrap
token. Compose runs as UID/GID `1000`; set `OLP_UID` and `OLP_GID` in `.env`
when the host user differs. If `POSTGRES_PASSWORD` contains reserved URL
characters, keep its RFC 3986 encoded form in
`POSTGRES_PASSWORD_URL_ENCODED`.

Open `http://localhost:8080`, paste the value in
`deploy/secrets/olp_bootstrap_token` into the first-run setup form, and create
the installation owner. Then recreate the application without the bootstrap
overlay and retire the token:

```bash
docker compose --env-file .env -f deploy/compose.yaml up -d --force-recreate olp
./scripts/retire-compose-bootstrap-secret.sh
```

Use `deploy/compose.yaml` alone for later release-image restarts and upgrades.
Contributors running a source build must keep the build overlay and `--build`
on those commands. See [`deploy/secrets/README.md`](deploy/secrets/README.md)
for rotation and file-backed connector details.

## Your first request

Sign in to the console, open **Keys**, and create a key allowed on at least
one route. The secret is shown once, when the key is created; copy it then,
because the installation stores only its hash.

```bash
export OLP_API_KEY="<the key secret the console showed>"
```

Send a chat completion to the OpenAI-compatible surface, using the slug of a
route the key is allowed on:

<!-- readme:first-request:curl -->

```bash
curl -sS http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $OLP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "my-route",
    "messages": [{"role": "user", "content": "Say hello"}]
  }'
```

The answer is an OpenAI chat completion. The assistant text is in
`choices[0].message.content`, `finish_reason` is `stop` when the provider
completed the answer, and `usage` carries the accounted token counts:

```json
{
  "id": "chatcmpl-9e0f2a1b",
  "object": "chat.completion",
  "created": 1756512000,
  "model": "my-route",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "Hello!"},
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 7,
    "completion_tokens": 5,
    "total_tokens": 12
  }
}
```

Add `"stream": true` to receive the answer incrementally:

<!-- readme:first-request:curl-stream -->

```bash
curl -sSN http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $OLP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "my-route",
    "stream": true,
    "messages": [{"role": "user", "content": "Say hello"}]
  }'
```

A streaming response is `text/event-stream`. Each `data:` line carries one
chunk whose `choices[0].delta` holds a fragment of the answer; concatenating
the `delta.content` fragments reconstructs the whole reply. Exactly one chunk
reports a non-null `finish_reason` — `stop` for a completed answer — and the
stream ends with the `data: [DONE]` sentinel.

The `model` value is always a route slug, never a provider model name: a
route is the unit of policy, carrying provider selection, failover, key
permissions, and pricing, so direct provider/model addressing is
intentionally unavailable. The slugs a key may use are listed by
`GET /v1/models` and in the console under **Routes**.

### Official SDKs

The official OpenAI, Anthropic, and Google GenAI clients need only a base URL
and the gateway key. Each surface is a different base on the same origin.

<!-- kept in sync with tests/sdk-smoke-python/smoke.py -->

```python
import os

import anthropic
import openai
from google import genai
from google.genai import types

ORIGIN = "http://localhost:8080"
API_KEY = os.environ["OLP_API_KEY"]

openai_client = openai.OpenAI(api_key=API_KEY, base_url=f"{ORIGIN}/v1")
completion = openai_client.chat.completions.create(
    model="my-route",
    messages=[{"role": "user", "content": "Say hello"}],
)
print(completion.choices[0].message.content)

anthropic_client = anthropic.Anthropic(
    api_key=API_KEY,
    base_url=f"{ORIGIN}/anthropic",
)
message = anthropic_client.messages.create(
    model="my-route",
    max_tokens=32,
    messages=[{"role": "user", "content": "Say hello"}],
)
print(message.content[0].text)

google_client = genai.Client(
    vertexai=False,
    api_key=API_KEY,
    http_options=types.HttpOptions(
        base_url=f"{ORIGIN}/gemini",
        api_version="v1beta",
    ),
)
response = google_client.models.generate_content(
    model="my-route",
    contents="Say hello",
)
print(response.text)
```

<!-- kept in sync with tests/sdk-smoke/smoke.mjs -->

```javascript
import Anthropic from '@anthropic-ai/sdk';
import { GoogleGenAI } from '@google/genai';
import OpenAI from 'openai';

const origin = 'http://localhost:8080';
const apiKey = process.env.OLP_API_KEY;

const openai = new OpenAI({ apiKey, baseURL: `${origin}/v1` });
const completion = await openai.chat.completions.create({
  model: 'my-route',
  messages: [{ role: 'user', content: 'Say hello' }]
});
console.log(completion.choices[0].message.content);

const anthropic = new Anthropic({ apiKey, baseURL: `${origin}/anthropic` });
const message = await anthropic.messages.create({
  model: 'my-route',
  max_tokens: 32,
  messages: [{ role: 'user', content: 'Say hello' }]
});
console.log(message.content[0].text);

const google = new GoogleGenAI({
  apiKey,
  apiVersion: 'v1beta',
  httpOptions: { baseUrl: `${origin}/gemini`, apiVersion: 'v1beta' }
});
const response = await google.models.generateContent({
  model: 'my-route',
  contents: 'Say hello'
});
console.log(response.text);
```

## Interfaces

All public interfaces share one origin — `http://localhost:8080` by default:

| Interface | Path |
|---|---|
| Console | `/` |
| Management API | `/api/v1` |
| OpenAI-compatible API | `/v1` (SDK compatibility alias) and `/openai/v1` (explicit protocol prefix) |
| Anthropic-compatible API | `/anthropic/v1` |
| Gemini-compatible APIs | `/gemini/v1` and `/gemini/v1beta` |
| Management OpenAPI | `/api/v1/openapi.json` — [tracked schema](openapi/management.json) |

Liveness, readiness, and metrics are private endpoints on
`OLP_OBSERVABILITY_LISTEN_ADDR` (default `127.0.0.1:9090`):
`/health/live`, `/health/ready`, and `/metrics`. Compose does not publish port
9090; public requests to these paths return 404.

### OpenAI compatibility

Both registered OpenAI base URLs work with the official OpenAI JavaScript and
Python SDKs and accept a trailing slash:

| Base URL | Meaning | Gateway credentials |
|---|---|---|
| `/v1` | OpenAI SDK-compatible alias | `Authorization: Bearer <OLP key>` or `x-litellm-api-key: <OLP key>` |
| `/openai/v1` | Explicit protocol-prefixed OpenAI surface | `Authorization: Bearer <OLP key>` or `x-litellm-api-key: <OLP key>` |

`x-litellm-api-key` accepts the raw key or `Bearer <OLP key>`. When it is
present, it is authoritative: an invalid value does not fall back to a valid
native credential, and two different valid OLP credentials are rejected. A
valid gateway key may coexist with a separate, non-OLP `Authorization` value.

These are explicit aliases, not wildcard forwarding. Gateway-only deployments
return 404 for bare root paths such as `/chat/completions` or `/models`; when
the management console is enabled, unmatched bare paths can instead serve the
console SPA fallback for deep links. Base paths without a registered operation
and unknown `/v1/*` or `/openai/*` paths are unsupported and return 404; they
are never passed through to a provider. The compatibility suite covers the
implemented chat-completions, Responses, streaming, and model list/retrieve
operations, but this does not imply support for other OpenAI endpoints.

## Console

The SvelteKit console is compiled to static assets and served by the same Rust
process. It has no API proxy in `pnpm dev`; use a built console with the Rust
server for API-backed pages. Visible UI changes can regenerate the fixture
screenshots with `pnpm --dir console screenshots`.

## Documentation

| Document | Purpose |
|---|---|
| [Architecture](docs/architecture.md) | Boundaries, runtime publication, endpoint/provider policy, limits, certification, and data safety |
| [Concepts](docs/concepts.md) | Routes and slugs, provider revisions and certification, runtime generations, keys and limits, attempts and pricing, data safety |
| [Configuration](docs/configuration.md) | Binary defaults, mounted secrets, Compose values, and provider presets |
| [Deployment](docs/deployment.md) | Helm install, edge routing, workers, observability, and readiness |
| [Operations](docs/operations.md) | Monitoring, recovery, upgrades, incidents, and master-key rotation |
| [Spend-budget recovery](docs/spend-budget-recovery.md) | How cost-budgeted keys initialize, fail closed, and recover after Valkey loss |
| [Provider compatibility matrix](docs/compatibility.md) | Which operations each provider kind serves natively, via translation, or refuses, and what each translation drops |
| [Contributing](CONTRIBUTING.md) | Toolchain, sources of truth, and validation |
| [Changelog](CHANGELOG.md) | Release notes, and the upgrade notes that go with them |
| [Security](SECURITY.md) | Supported releases and private vulnerability reporting |
| [Console development](console/README.md) | Frontend commands, boundaries, integration tests, and screenshots |
| [Compatibility and contract tests](tests/README.md) | Conformance, SDK, end-to-end, HA, and fuzz suites |
| [Compose secrets](deploy/secrets/README.md) | Bootstrap lifecycle, key rotation, and file-backed connectors |
| [Amazon Bedrock connector](crates/olp-engine/BEDROCK.md) | Authentication, transport behavior, and focused tests |

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) lists the pinned toolchain. Install the
console dependencies once, then run the standard local gate:

```bash
make console-install
make check
```

`make help` lists the locked CI-aligned targets, including coverage,
database-backed tests, contract tests, and generated-file checks. See
[`CONTRIBUTING.md`](CONTRIBUTING.md) before changing architecture or APIs.

## Security

Report vulnerabilities privately to `support@mail.tyk.sh`; see
[`SECURITY.md`](SECURITY.md). Do not include credentials or customer data.

## License

Licensed under the GNU Affero General Public License v3.0 only
(`AGPL-3.0-only`). See [LICENSE](LICENSE).
