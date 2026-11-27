# Getting Started

This guide installs OpenLLMProxy with Docker Compose, creates the first owner,
connects and activates a provider, publishes a route and makes a request through
that route. Development setup follows a different path and is covered in
[Contributing](../CONTRIBUTING.md#local-development).

You need Docker with Compose and either an upstream provider account or a
reachable self-hosted model endpoint. Run the commands from the repository root.
By the end of the guide, an API key should be able to call the published route,
and the console should show the resulting records under **Requests** and
**Usage**.

## Install OLP

Build and start the Compose installation.

```sh
docker compose -f deploy/compose.yaml -f deploy/compose.build.yaml up --build -d
```

The first start generates the master-key ring, the authentication HMAC key, the
database password and a one-time setup token into the `olp-secrets` and
`olp-database-secret` volumes. Later starts reuse them, so back these volumes up
with the database. The `olp` container reports unhealthy until the first
provider connection is activated, which publishes runtime generation 1 before
any route is published so the gateway has a valid runtime snapshot.

The default public origin is `http://localhost:8080`. To use another port or
address, copy `.env.example` to `.env`, set `OLP_HOST_PORT` or
`OLP_PUBLIC_ORIGIN`, and add `--env-file .env` to every Compose command.

`deploy/compose.yaml` already defaults to `ghcr.io/tyk-swe/olp:0.2.0`. To run
that published image instead of building locally, omit
`deploy/compose.build.yaml` and `--build` from the command. Set `OLP_IMAGE` in
`.env` only to override the pinned image.

This local installation is the quickstart path and not the complete production
topology. [Deployment](operate/deployment.md) covers production TLS, secrets,
database roles, private observability and separate process modes. The
[Configuration](operate/configuration.md) guide lists every supported variable.

## Create the first owner

Print the setup link and token.

```sh
docker compose -f deploy/compose.yaml exec olp olp setup-token
```

Open the printed link, paste the token and complete the first-owner form. The
token stops working once the owner exists, so it can stay mounted. Confirm that
the new owner can sign in before continuing.

## Connect and activate a provider

1. In **Providers**, choose **Add provider**. Select the vendor, endpoint and
   authentication method. Leave **Project** set to **Installation-wide** for
   this quickstart so **Compare models and create routes** can target the
   connection directly. If you prefer project isolation, create a project first
   and assign the connection to that project instead.
2. Choose **Save and test connection**. Run **Discover upstream models** or
   enter the exact upstream model identifiers, then enable the models this
   installation should serve. Some providers require an explicit probe model.
3. Certify the operation, protocol surface and mode that the route will serve
   by choosing **Validate N selected models** for bulk certification or
   **Server-certify capabilities** on an individual model. Certification can
   make a small billable upstream request.
4. Review the completed draft, choose **Test completed draft**, and then choose
   **Activate provider**. Discovery and model selection alone do not publish a
   provider connection. Activating the first provider publishes runtime
   generation 1 and makes the `olp` container report healthy.

For the example request below, choose a model certified for the OpenAI Responses
surface. [Provider routing](use/provider-routing.md#connect-and-qualify-models)
explains the full lifecycle. [Provider profiles](use/provider-profiles.md)
covers authentication and network choices, and
[Compatibility](use/compatibility.md) states the supported surfaces.

## Publish a route

For the **Installation-wide** provider connected above, publish a route through
the model comparison tool.

1. In **Models**, open **Compare models and create routes**.
2. Select the certified model and set a route slug such as `assistant`.
3. Choose **Create reviewed route drafts**, review the generated draft, and
   choose **Validate and publish reviewed routes**.

**Compare models and create routes** always creates installation-wide route
drafts, which fail with `422 target_project_mismatch` if the target provider
connection belongs to a project. If you placed the provider connection in a
project, or if you want to create one route right after activation via
**Build default route**, open **Routes** and choose **New route draft**
(`/routes/new`, titled **New route**). Select the same project as the provider
connection, set the slug to `assistant`, add the certified model target, create
the draft, validate it and activate it.

Clients use the route slug as the model name. Strict fidelity is the default
because it preserves the target provider's native contract. Choose transformed
fidelity only when the route must translate between dialects, redact content or
use an Automatic provider without a profile. The certified capabilities on the
route determine which requests it can accept.
[Route fidelity](use/provider-routing.md#route-fidelity) explains the choice in
more detail.

## Create an API key

In **API keys**, choose **New API key** and match the route's project scope.
Leave **Project** set to **Installation-wide** for an installation-wide route,
or select the route's project for a project-owned route. Grant the `inference`
scope and permission for `assistant`, or for a route group that contains it. Add
`models_read` only if the client needs model discovery through the gateway.

Store the one-time secret in the shell as `OLP_API_KEY`. OLP stores only a
digest and cannot show the secret again. [Access](use/access.md) covers key
expiry, rotation, network restrictions and budget controls.

## Make the first request

With the default Compose origin, send a minimal Responses request.

```sh
curl --fail-with-body http://localhost:8080/v1/responses \
  -H "Authorization: Bearer $OLP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"assistant","input":"Hello"}'
```

The same route can be called from JavaScript with the `openai` package installed
in the client application.

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

An Anthropic client uses `/anthropic` under the public origin. A Gemini client
uses `/gemini`. Bedrock SDK ingress uses `/bedrock` and its separate gateway
authentication contract in the
[Bedrock guide](use/providers/bedrock.md#bedrock-sdk-ingress). Qualified
configurations for other SDKs, frameworks and coding clients are collected in
[Clients](use/clients.md).
[Automation](operate/automation.md#configure-qualified-clients) can generate
qualified client setup from mounted keys.

Finish by opening **Requests** and **Usage** in the console. The records should
identify the `assistant` route, its provider attempt and the accounting observed
for the request.
