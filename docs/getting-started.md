# Getting Started

Install OLP, create the first owner, connect a provider and make a request through
a published route. You need Docker with Compose and an upstream account or
reachable self-hosted model endpoint. Development setup is in
[Contributing](../CONTRIBUTING.md#local-development).

## Install

From the repository root, build and start the local Compose installation:

```sh
cp .env.example .env
./scripts/prepare-compose-secrets.sh
docker compose --env-file .env \
  -f deploy/compose.yaml -f deploy/compose.build.yaml \
  -f deploy/compose.bootstrap.yaml up --build -d
```

The default public origin is `http://localhost:8080`. Set `OLP_PUBLIC_ORIGIN`
and the host port in `.env` if needed, and open the configured origin. To use a
published 0.x image, set `OLP_IMAGE` and omit the build overlay and `--build`.
See [Deployment](operate/deployment.md) for production TLS, secrets, database
roles, private observability and separate process modes; the
[configuration reference](operate/configuration.md) lists all variables.

## Create the first owner

Use the value in `deploy/secrets/olp_bootstrap_token` in the first-owner setup
form. If you configured `OLP_COMPOSE_SECRETS_DIR`, use the token there. Create the
owner and verify sign-in at the configured public origin.

Recreate the initialized application without the bootstrap overlay, preserving
the build overlay used above, then retire the token:

```sh
docker compose --env-file .env \
  -f deploy/compose.yaml -f deploy/compose.build.yaml \
  up -d --force-recreate olp
./scripts/retire-compose-bootstrap-secret.sh
```

Keep the master-key ring and authentication HMAC key for this installation.
The [Compose secrets guide](../deploy/secrets/README.md#bootstrap-token-lifecycle)
covers custom secret directories, token retirement and later restarts.

## Connect and activate a provider

1. In **Providers → Add provider**, choose the vendor, endpoint and authentication.
   Put the connection in the project your consumer key will use.
2. Test the connection, discover models or enter exact upstream identifiers, then
   enable the models you need. Some providers require an explicit probe model.
3. Use **Validate models in bulk** to certify the operation, surface and mode you
   intend to serve. Certification may make a small billable request.
4. Review and activate the completed draft. Discovery alone does not publish it.

Choose an OpenAI Responses-capable model for the request below. See
[provider routing](use/provider-routing.md#connect-and-qualify-models),
[profiles](use/provider-profiles.md) and [compatibility](use/compatibility.md)
for other providers and native surfaces.

## Publish a route

In **Models → Compare models and create routes**, select the certified model,
set a route slug such as `assistant`, and review the generated route draft.
Publish it in the same project. Requests use this slug as their model name.

Strict fidelity is the default. Choose transformed fidelity when translating
between dialects, redacting content or using an Automatic provider without a
profile. The route's certified capabilities determine which requests it accepts;
see [route fidelity](use/provider-routing.md#route-fidelity).

## Create an API key

In Access, create an API key in the route's project with `inference` scope and
permission for `assistant` (or an appropriate route group). Add `models_read`
if your client needs discovery. Save the one-time key securely and set it as
`OLP_API_KEY` in your shell. [Access](use/access.md) covers expiry, rotation,
network restrictions and budget controls.

## Make the first request

With the default Compose origin:

```sh
curl --fail-with-body http://localhost:8080/v1/responses \
  -H "Authorization: Bearer $OLP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"assistant","input":"Hello"}'
```

For JavaScript, install the `openai` package in your application and use:

```javascript
import OpenAI from 'openai';

const client = new OpenAI({
  apiKey: process.env.OLP_API_KEY,
  baseURL: 'http://localhost:8080/v1'
});
const response = await client.responses.create({
  model: 'assistant',
  input: 'Hello'
});
console.log(response.output_text);
```

Use `/anthropic` as the Anthropic SDK base URL or `/gemini` for Gemini. Bedrock
uses `/bedrock` with separate gateway authentication; follow its
[SDK ingress guide](use/providers/bedrock.md#bedrock-sdk-ingress).
[Clients](use/clients.md) provides qualified configurations, and
[Automation](operate/automation.md#configure-qualified-clients) generates setup
from mounted keys. In the console, inspect request history and usage to verify
the route, attempts and observed accounting.
