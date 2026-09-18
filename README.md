# OpenLLMProxy

OpenLLMProxy is a self-hosted AI gateway and control plane built with Go,
SvelteKit, PostgreSQL, and Valkey. It routes native OpenAI, Anthropic, and Gemini
SDK requests across OpenAI, Anthropic, Gemini, Vertex AI, Amazon Bedrock, Azure
OpenAI, and reviewed OpenAI-compatible endpoints.

The Go release requires a fresh installation. Rust 2.x and 3.x databases
are rejected before any migration runs. Storage uses `olp_go`; there is no
Rust-to-Go data migration. Provision a separate database and keep the old installation
and its backups until you have verified the replacement.

## Develop locally

Install Go 1.27.1, a C compiler/linker and glibc headers, Node.js 26, pnpm 11,
Docker Compose, and PostgreSQL 18 client tools, then run:

```sh
make setup
make dev
```

Open http://localhost:5173. Use the token in `.local/go-secrets/bootstrap.token` to
create the first owner. Vite serves the console with hot reload and proxies API,
OIDC callback, and streaming requests through that same origin. PostgreSQL and
Valkey use isolated development volumes and loopback ports 54321 and 63791.

`make check` runs the required local checks. `make integration` runs the
service, recovery, SDK, and Chromium journey suites. CI also qualifies
dependencies. See
[CONTRIBUTING.md](CONTRIBUTING.md) for commands and the TypeScript 6.0 support
exception, and [the architecture map](docs/architecture.md) for feature ownership.

## Install

Build the image locally, or select a published 3.x image through `OLP_IMAGE`.
Use fresh PostgreSQL and Valkey storage; do not reuse Rust storage volumes.

```sh
cp .env.example .env
./scripts/prepare-compose-secrets.sh
docker compose --env-file .env -f deploy/compose.yaml -f deploy/compose.build.yaml -f deploy/compose.bootstrap.yaml up --build -d
```

Visit the configured `OLP_PUBLIC_ORIGIN` and use
`deploy/secrets/olp_bootstrap_token` for first-owner setup. After setup, recreate
the application without the bootstrap overlay and run
`./scripts/retire-compose-bootstrap-secret.sh`.

The console walks through connection, model discovery and capability
certification, then activation. Create a route targeting activated provider
models, publish it, and issue an API key with the required scope and route
allowlist. Credentials are write-only. Provider and route changes use ETags,
immutable revisions, and explicit activation. [Provider routing](docs/provider-routing.md)
adds vendor-based connections, credential pools, bulk model validation and route
creation, and bounded price, latency, throughput, and privacy preferences.

For production, see [deployment](docs/deployment.md),
[configuration](docs/configuration.md), and [operations](docs/operations.md).
Gateway, control, and worker modes can run as separate processes and replicas.
PostgreSQL owns durable state; Valkey coordinates distributed limits and event
delivery. Workers recover accounting and media jobs after dependency failures.

## Make an SDK request

The OpenAI base URL is the deployment origin plus `/v1`. The model is a
published route slug:

```javascript
import OpenAI from 'openai';

const client = new OpenAI({
  apiKey: process.env.OLP_API_KEY,
  baseURL: 'http://localhost:5173/v1'
});
const response = await client.responses.create({
  model: 'assistant',
  input: 'Hello'
});
console.log(response.output_text);
```

Use `/anthropic` as the Anthropic SDK base URL and `/gemini` as the Gemini SDK
base URL. Native SDK authentication, unary responses, and streaming are
preserved. `/openai/v1` and `x-litellm-api-key` are retired.

| Interface | Path |
| --- | --- |
| Management API | `/api/v3` |
| Management OpenAPI | `/api/v3/openapi.json` |
| OpenAI | `/v1` |
| Anthropic | `/anthropic/v1` |
| Gemini | `/gemini/v1beta` and `/gemini/v1` |
| Private liveness and readiness | Separate observability listener on port 9090 |

Supported operations include generation and token counting, embeddings,
moderation, image generation and editing, speech and transcription, and durable
video jobs, subject to the selected provider's certified capabilities. Routes
control eligibility, priorities, weighted selection, attempt limits, and
timeouts. API keys control permissions, rate limits, concurrency, and exact
daily and monthly cost budgets.

The console includes provider and route history, access and OIDC management,
usage and pricing, request metadata, media jobs, and health. Persisted telemetry
excludes prompts, outputs, credentials, and uploaded content.

## Documentation

| Guide | Covers |
| --- | --- |
| [Provider routing](docs/provider-routing.md) | Vendors, credential pools, model facts, policies, and coordinated upgrade |
| [Concepts](docs/concepts.md) | Routes, provider revisions, keys, usage, and privacy |
| [Compatibility](docs/compatibility.md) | Supported endpoints and translation limits |
| [Deployment](docs/deployment.md) | Production topology, secrets, and capacity |
| [Configuration](docs/configuration.md) | Environment variables and CLI settings |
| [Operations](docs/operations.md) | Monitoring, recovery, and upgrades |
| [Production contracts](docs/production-guarantees.md) | Guarantees, assumptions, and qualification limits |
| [Go gateway](docs/go-gateway.md) | Go providers, protocols, media, routing, and worker plane |
| [Go access](docs/go-access.md) | Go installation, identity, and access control |
| [Go roadmap](docs/roadmap/) | Go rewrite milestones and qualification status |
| [Contributing](CONTRIBUTING.md) | Development, checks, and releases |

## Operations

Back up a drained 3.0 installation and restore into an empty database:

```sh
OLP_DATABASE_URL=postgres://... OLP_BACKUP_TRAFFIC_QUIESCED=true ./scripts/backup.sh backups
OLP_RESTORE_DATABASE_URL=postgres://... OLP_RESTORE_VALKEY_ISOLATED=true ./scripts/restore.sh /path/printed/by/backup.sh
```

Pass the dump path printed by the backup script to restore. Backups include a
checksum and manifest and preserve the installation identity.
Mount the original master-key ring and authentication HMAC key, configure a
separate empty Valkey service, and use a database role with CREATEDB for restore
validation. Retain the keys separately from the backup.
See [operations](docs/operations.md) for quiescing, recovery, and key rotation.

OpenLLMProxy is licensed under AGPL-3.0-only. Report vulnerabilities through
[SECURITY.md](SECURITY.md).
