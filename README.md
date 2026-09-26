# OpenLLMProxy

OpenLLMProxy is a self-hosted AI gateway and control plane built with Go,
SvelteKit, PostgreSQL, and Valkey. It routes OpenAI, Anthropic, Gemini, and
Bedrock client requests across certified provider models. Providers include
OpenAI, Anthropic, Gemini, Vertex AI, Amazon Bedrock, Azure OpenAI, and reviewed
OpenAI-compatible endpoints.

OpenLLMProxy 0.1.0 is a work in progress. During 0.x it makes no compatibility,
upgrade, mixed-version or rollback promises: any release may change the
management API, configuration and storage. See
[ADR 0004](docs/adr/0004-no-compatibility-promises-during-0x.md).

## Develop locally

Install the [development prerequisites](CONTRIBUTING.md), then run:

```sh
make setup
make dev
```

Open http://127.0.0.1:5173 and use `.local/secrets/bootstrap.token` to create
the first owner. Vite serves the console with hot reload and proxies the
configured API paths to Go. Restart after backend edits.

`make test` runs local Go, console, and script tests. `make check` adds contract
generation and static checks; `make integration` runs service, SDK, browser, and
recovery suites. See [Contributing](CONTRIBUTING.md) for the full workflow.

## Install

Build the image locally with Compose:

```sh
cp .env.example .env
./scripts/prepare-compose-secrets.sh
docker compose --env-file .env \
  -f deploy/compose.yaml -f deploy/compose.build.yaml \
  -f deploy/compose.bootstrap.yaml up --build -d
```

Visit the configured `OLP_PUBLIC_ORIGIN` and use
`deploy/secrets/olp_bootstrap_token` for first-owner setup. After setup,
[recreate the application without the bootstrap overlay and retire the token](deploy/secrets/README.md#bootstrap-token-lifecycle).
To use a published 0.x image, set `OLP_IMAGE` and omit the build overlay and
`--build`.

In the console, connect a provider, discover and certify models, and activate
it. Create and publish a route targeting those models, then issue an API key in
the same project with the required scopes and route access. A route is strict
unless you declare it transformed; declare it transformed when it translates
between dialects, redacts content or uses a provider without a provider
profile. See [provider routing](docs/provider-routing.md) for route fidelity,
credential pools, bulk workflows, and price, latency, throughput, and privacy
preferences.

For production, follow [deployment](docs/deployment.md) and
[configuration](docs/configuration.md). Gateway, control, and worker modes can
run separately; workers persist accounting and reconcile media jobs and budgets.

## Make an SDK request

Install the `openai` JavaScript package and set `OLP_API_KEY` to a key with
`inference` scope. The model is a published route slug:

```javascript
import OpenAI from 'openai';

const client = new OpenAI({
  apiKey: process.env.OLP_API_KEY,
  baseURL: 'http://127.0.0.1:5173/v1'
});
const response = await client.responses.create({
  model: 'assistant',
  input: 'Hello'
});
console.log(response.output_text);
```

Use `/anthropic` as the Anthropic SDK base URL and `/gemini` as the Gemini SDK
base URL. Bedrock uses `/bedrock` and separate gateway authentication; see the
[Bedrock guide](docs/providers/bedrock.md#bedrock-sdk-ingress), including proxy
requirements.

| Interface | Path |
| --- | --- |
| Management API and OpenAPI | `/api/v1` and `/api/v1/openapi.json` |
| OpenAI | `/v1` |
| Anthropic | `/anthropic/v1` |
| Gemini | `/gemini/v1beta` and `/gemini/v1` |
| Bedrock | `/bedrock` |
| Private health and metrics | Separate observability listener on port 9090 |

The [compatibility matrix](docs/compatibility.md) lists generation, token
counting, embeddings, rerank, moderation, media, and qualified file, batch,
realtime, and stored-response operations. Support depends on the provider,
model, and certified capability. Persisted telemetry excludes prompts, outputs,
credentials, and uploaded content; opt-in provider state may retain content
upstream.

## Documentation

| Guide | Covers |
| --- | --- |
| [Concepts](docs/concepts.md) | Projects, routes, revisions, keys, budgets, and privacy |
| [Provider routing](docs/provider-routing.md) | Onboarding, credential pools, model facts, and selection policy |
| [Compatibility](docs/compatibility.md) | Endpoints, supported providers, and translation limits |
| [Deployment](docs/deployment.md) | Production topology, secrets, capacity, and edge routing |
| [Configuration](docs/configuration.md) | Variables, CLI settings, and configuration promotion |
| [Access control](docs/access.md) | Identity, projects, management tokens, and account recovery |
| [Gateway execution](docs/gateway.md) | Admission, attempts, content policies, and durable media |
| [Operations](docs/operations.md) | Monitoring, recovery, and versions |
| [Production contracts](docs/production-guarantees.md) | Guarantees, assumptions, and qualification limits |
| [Contributing](CONTRIBUTING.md) | Setup, tests, architecture, and releases |

## Operations

Follow the [backup and restore procedure](docs/operations.md#backup-and-restore)
for drained backups, original-key recovery, and isolated replacement storage.
See [spend recovery](docs/spend-budget-recovery.md) for budget initialization
and reconciliation, and
[key rotation](docs/access.md#master-key-rotation-and-recovery) for
encryption-key maintenance.

OpenLLMProxy is licensed under AGPL-3.0-only. Report vulnerabilities through
[the security policy](SECURITY.md); community participation follows the
[Code of Conduct](CODE_OF_CONDUCT.md).
