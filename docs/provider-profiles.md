# Provider profiles and connection configuration

A versioned provider profile selects an API dialect, hosting wrapper,
authentication combination and operation schemas. It is separate from a model
binding and credential slot. Profile selection does not certify an entire model
interaction or establish quality parity.

A provider that omits `profile_id` and `profile_revision` is an Automatic
provider: its endpoints follow from its provider kind, including the
Responses-to-Chat fallback for chat-only vendors, and only
[transformed routes](provider-routing.md#route-fidelity) can use it. Select
both fields to use a profile, which strict routes require. Unknown revisions,
incompatible connector/auth combinations, a wrong Vertex publisher path, and
undeclared operations fail validation. OpenAI Chat and Responses are separate
generation dialects; a Responses profile cannot fall back to the Chat endpoint.

`GET /api/v1/provider-profiles` returns profile revisions, operation dialects,
allowed semantic headers/query settings and JSON schemas for operation defaults.
Those schemas also describe the controls the console can present. Each entry's
`strict` reports whether the profile may serve
[strict routes](provider-routing.md#route-fidelity). Model certification and
credential-slot validation remain explicit activation gates.
Connectivity/certification is not empirical evidence of model intelligence parity.

The catalogue also lists the profiles of approved [provider plugins](plugins.md)
under the `plugin` provider kind. A plugin profile's revision is the digest of
the plugin module, or of an unconfined plugin's executable, that supplies it, so
a provider pins that build. It serves one built-in dialect's generation
operation at the address its plugin declares, shares that dialect's revision and
semantic headers and query settings, and authenticates with a static credential
that its declared headers and query parameters place, or that its
[signing hook](plugins.md#signing-hooks) signs with, or with a
[grant](plugins.md#grant-enrollment) when it declares one. Its `options_schema`
describes the options a provider using it sets, which the hosting adaptation
places too. A plugin profile that also envelopes or rewrites the dialect's
bodies or forces upstream streaming, or whose unconfined plugin
[carries its traffic](plugins.md#carrying-traffic), is not `strict`. Its entry
reports `model_discovery: true` when the plugin declares the upstream's model
listing; otherwise operators declare its providers' models.

| Profile | Native generation address / contract |
| --- | --- |
| `openai-chat`, `compatible-chat` | `/chat/completions` under the configured base. |
| `openai-responses`, `compatible-responses` | `/responses` under the configured base. |
| `anthropic-messages` | `/messages`, Anthropic version `2023-06-01`. |
| `gemini-generation` | `/models/{model}:generateContent` or `:streamGenerateContent`. |
| `azure-legacy-chat` | `/openai/deployments/{deployment}/chat/completions` with a dated API version. |
| `azure-legacy-responses` | `/openai/responses` with a dated API version and deployment in the body. |
| `azure-v1-chat`, `azure-v1-responses` | `/openai/v1/chat/completions` or `/openai/v1/responses`; no dated API-version setting. |
| `vertex-gemini` | Configured project/location, Google publisher and Gemini dialect. |
| `vertex-anthropic` | Anthropic publisher, `:rawPredict` / `:streamRawPredict`; body version `vertex-2023-10-16`, model selected in URL. |
| `vertex-openai` | `/endpoints/openapi/chat/completions` under the configured project/location; models named `publisher/model`, such as `google/gemini-2.5-flash` or `meta/llama-3.3-70b-instruct-maas`. |
| `bedrock-converse` | `/model/{model}/converse` or `/converse-stream`. |
| `bedrock-anthropic-invoke` | `/model/{model}/invoke` or `/invoke-with-response-stream`; body version `bedrock-2023-05-31`. |
| `bedrock-invoke` | Qualified model-specific native Invoke surface; no generation/Chat fallback. |
| `sagemaker-openai-chat` | `/endpoints/{endpoint}[/inference-components/{component}]/openai/v1/chat/completions`; see [SageMaker AI](providers/sagemaker.md). |

Each built-in composition currently has OLP profile revision `1`. This identifies
OLP's component composition, not an immutable provider/model release. Undated
provider APIs and moving model aliases remain mutable. Gemini Interactions and
Live require their own operation/lifecycle implementations and profiles; neither
is represented by GenerateContent here.

Azure v1 accepts a resource origin or a base ending in `/openai/v1`. The Entra
scope for this profile is `https://ai.azure.com/.default`; the `azure-legacy-*`
profiles use `https://cognitiveservices.azure.com/.default`. Vertex endpoints
must match the configured project, location and profile publisher, or for
`vertex-openai` the project's `endpoints/openapi` address. When a Vertex provider
names no endpoint, OLP derives the address from the profile.

Azure AI Foundry Models, such as DeepSeek, Grok and Llama deployments, use the
`azure-v1-chat` and `azure-v1-responses` profiles at the resource's
`https://{resource}.services.ai.azure.com` origin, with the deployment name as
the model. Microsoft has deprecated the separate Model Inference API under
`/models`, so OLP has no profile for it. Bedrock
signing happens only after the final URL, body and semantic headers have been
constructed. Anthropic Invoke streaming unwraps bounded AWS event envelopes into
native Anthropic events; it does not decode the stream as Converse or
manufacture terminal events.

The `cohere-embed-v2` and `cohere-rerank-v2` profiles compose the existing
`openai_compatible` direct HTTP hosting and API-key authentication with distinct
[Cohere Embed v2](https://docs.cohere.com/reference/embed) and
[Rerank v2](https://docs.cohere.com/reference/rerank) operation codecs. Choose the
`cohere-native-v2` preset at `https://api.cohere.ai/v2`, or a trusted custom
v2-compatible base; native requests use `/native/cohere-embed-v2/models/{route}`
or `/native/cohere-rerank-v2/models/{route}`. The `cohere` preset uses the
`/compatibility/v1` endpoint instead. Selecting either native profile with
that official compatibility endpoint fails configuration validation instead of
silently calling `/compatibility/v1/embed` or `/rerank`.

Embed v2 retains original `texts`, image data URIs or ordered multimodal
`inputs`, task type, `truncate`, dimension and requested float/int8/uint8/
binary/ubinary/base64 storage. Multi-type or non-float storage requires the
explicit `raw-vector-storage/1` client contract. Rerank v2 retains original
query/documents, `top_n`, per-document token cap, priority, returned indices,
native score spelling/order/ties and provider search-unit billing. Unknown
native extensions remain source-exact where policy coverage permits them;
foreign aliases and uninspectable content under restrictive policy refuse
before dispatch. Cohere's v2 API does not define sparse or token-multivector
output; TEI remains the scoped native contract for those layouts. No
cross-dialect mapping or live-model quality claim is implied.

## Native generation dialects

Some vendor generation APIs have no counterpart in another dialect. Clients
speak them as they are, at `POST /native/{dialect}/models/{route}`, on strict
routes whose targets are certified for `generation` on the `native` surface:

| Dialect | Profile | Upstream |
| --- | --- | --- |
| `mistral-fim` | `mistral-fim`, with the `mistral` preset | [`/v1/fim/completions`](https://docs.mistral.ai/api/endpoint/fim) |
| `cohere-chat-v2` | `cohere-v2`, the `cohere-native-v2` preset's profile | [`/v2/chat`](https://docs.cohere.com/reference/chat) |

OLP validates the envelope, binds the route's model in place of the body's
`model`, which may be absent or name the route, and forwards everything else
unchanged: Cohere's documents, citation options, tools and thinking reach
Cohere, and its citations, tool plans and documents return byte for byte. The
result and every streamed event are checked against the dialect's grammar and
metered: fill-in-the-middle as chat completion chunks ending in `[DONE]`, and
Cohere by the billed units its `message-end` event reports, which leave out its
own preamble. A stream that ends before its terminal event is truncated.

A native generation dialect is never translated. Its profile serves no other
surface, and an OpenAI, Anthropic or Gemini request to its route finds no
eligible target; an automatic `cohere-native-v2` provider has no Chat
Completions to send at all, so it does not generate. Input content policies
cannot inspect these dialects and refuse them.

## Defaults, bindings and semantic configuration

An example configuration fragment for native Anthropic generation:

```json
{
  "kind": "anthropic",
  "profile_id": "anthropic-messages",
  "profile_revision": "1",
  "auth_mode": "api_key",
  "endpoint": "https://api.anthropic.com/v1",
  "options": {
    "semantic_headers": { "Anthropic-Version": "2023-06-01" },
    "operation_defaults": {
      "generation": {
        "dialect": "anthropic-messages",
        "values": { "temperature": 0.5 }
      }
    },
    "bindings": {
      "logical-model": {
        "model": "configured-native-model-id",
        "principal_id": "operator-declared-upstream-account",
        "defaults": {
          "generation": {
            "dialect": "anthropic-messages",
            "values": { "temperature": 0.25 }
          }
        }
      }
    }
  }
}
```

The provider default is inherited first, followed by the selected binding's
member replacement. An explicit caller member wins, including native null, zero,
false or an empty array. Array/tool/schema definitions replace atomically;
configuration layers do not recursively merge them. To reset a configured
default, remove that member in the replacement configuration. A raw `null` value
inside `values` is an actual native null, not a reset instruction. A null default
definition, ambiguous duplicate JSON member or case-aliased configuration field
is rejected before typed decoding can erase the distinction.

`values` is checked against the operation/dialect control schema. Explicit native
extensions belong in `native_options`; they remain data and cannot replace model,
input envelope, destination, authentication, routing or durable-resource authority.
Shared/native member collisions are errors, including collisions across provider
and binding layers. `parameter_defaults` belong to Automatic providers and
cannot be mixed with explicit profiles. Prepared invocation provenance records
which omissions were actually filled and where the configured default came from.
Shared token admission uses the same prepared bounds sent upstream; it does not
shrink requested controls.

For Gemini embeddings, native defaults such as `taskType` and
`outputDimensionality` apply to each native batch member, preserving input order.
Vertex embedding defaults use its atomic `parameters` member. Media defaults
retain original JSON presence and exact native extension numbers; multipart
values need a qualified representation and delivery-mode changes are rejected.

Bindings can declare model/deployment, principal, snapshot, region and resource
scope. Identity facts without independent observation remain operator declarations;
a secret version is not proof of an upstream account, while a grant's
[observed principal](plugins.md#re-enrolling-and-the-observed-principal)
replaces a declared principal. Binding/semantic/profile changes are visible
separately in revision diffs. Credential rotation preserves semantic
configuration; changes to a declared principal or resource scope are
serving-identity changes rather than a secret-refresh shortcut.

Semantic headers are independent from encrypted API credentials. Profile allowlists
reserve authorization, host, hop-by-hop, tenant, tracing and security-sensitive
routing controls. Invalid HTTP header values are rejected without echoing them.
Google profiles allow the schema-owned `$xgafv` query setting (`1` or `2`); a
legacy Azure `api-version` query must match its configured API revision. No
arbitrary query setting can change destination or credential scope.

## Secure connection options

`options.network` is optional. Its fields are:

| Field | Contract |
| --- | --- |
| `proxy_url` | HTTP/HTTPS CONNECT or SOCKS5 proxy origin, with no userinfo, path, query or fragment. |
| `trust_roots_pem` | Public certificate PEM augmenting system trust; private-key blocks are rejected. |
| `credential_id` | Provider-owned encrypted network credential version UUID. |
| `connect_timeout_ms` | DNS, TCP and proxy establishment budget, 1–120000 ms. |
| `tls_handshake_timeout_ms` | TLS phase budget, 1–120000 ms. |
| `response_header_timeout_ms` | Response-header deadline, 1–600000 ms. |
| `idle_conn_timeout_ms` | Idle pooled-connection lifetime, 1–3600000 ms. |
| `max_idle_conns`, `max_idle_conns_per_host`, `max_conns_per_host` | Positive bounded pool settings, at most 4096. A pool that sets none has 128, 16 and 64. |

A provider has one of two kinds of connection pool, and which one depends on
whether `options.network` exists. A provider with no `options.network` and no
profile shares the gateway's own transport: it keeps at most 16 idle connections
to a host and puts no limit on the connections it opens to one. Setting any
`options.network` field, even `connect_timeout_ms` alone, or selecting a provider
profile, gives the provider a pool of its own for each credential slot, and that
pool is capped at 64 connections to a host, 16 of them idle and 128 idle in all,
unless the settings above say otherwise. A request waits inside the transport for
a connection the cap does not allow, with no error, no admission rejection and no
open circuit, so over HTTP/1.1 the latency of everything past the 64th request in
flight at one slot grows by the queue. A provider that carries more concurrent
streams than that raises `max_conns_per_host` and `max_idle_conns_per_host` (a
pool never takes more than 4,096) or spreads its traffic over more credential
slots or providers, whose pools are separate. See
[provider connection capacity](deployment.md#provider-connection-capacity).

Both the proxy and every destination DNS answer pass the existing egress policy.
CONNECT and SOCKS requests carry locally validated destination IPs while preserving
target Host/SNI. Remote DNS (`socks5h`) is refused because local DNS pinning cannot
control a proxy's resolver. HTTP proxies must support CONNECT even for HTTP
targets. Environment proxy variables are ignored. Plain HTTP proxy origins require
the existing operator host exception. TLS verification, TLS >=1.2, redirect
refusal, bounded headers/bodies/events and cancellation remain enforced.

Custom roots apply to HTTPS proxy and target TLS independently. A client
certificate is offered only to the target, never the proxy. Proxy authentication
is confined to tunnel establishment. Reusable pools are bounded and isolated by
provider/serving revision/credential scope, even when network settings match.
A cached connection is never a substitute for current credential authority.

Create network material with `POST /api/v1/providers/{id}/network-credentials`
using the current provider `If-Match` and an `Idempotency-Key`. Its `credential`
string contains a JSON object with `proxy_username`/`proxy_password` and/or
`client_certificate_pem`/`client_key_pem`. The endpoint encrypts it through the
existing secret authority and returns only a reference. Patch that reference into
`options.network.credential_id`, certify/validate, then activate normally. A
network credential cannot be selected as an API credential slot or borrowed from
another provider. Listing credentials returns metadata; revocation advances the
same current authority used by inference and retained resource recovery.

Configuration export replaces the environment-specific UUID with
`network_credential_ref` (the provider name followed by `/network`). Import uses
that reference in `secret_bindings`, creates or reuses a destination-owned
network credential, and retains the provider/route as drafts. Network and API
credential references must be distinct; an ambiguous reference is rejected.
Exports never contain private keys, proxy passwords or source-installation UUIDs.
Export, plan and apply perform no provider call. Probes and certifications remain
separate actions and may execute their documented inference probes.

The same network options apply to discovery/certification, generation,
non-generation media, multipart resources, retained resource retrieval and
WebSocket realtime connections. Pinned revisions retain their network/profile
configuration, but current credential revocation still wins. Full multi-turn
continuation admission is an additional interaction/lifecycle contract, not a
claim implied by this configuration feature.

## Qualification and extension

Controlled tests cover every registered composition's final path, headers,
wrappers and signing; full public mTLS/credential rotation/revocation and portable
configuration round trips; HTTP/HTTPS/SOCKS destination protection and pool
isolation; media/resource/realtime TLS transport; and preserve-or-reject defaults.
They are deterministic fixture evidence, not live-provider or quality evidence.

A trusted in-process `connectors.RegisterProfile` can add a provider using an
existing component composition and model bindings. The registry validates the
composition. Executable configuration comes from [provider plugins](plugins.md)
instead: WASM modules an owner installs, whose code OLP runs confined, or, in
the experimental [unconfined tier](plugins.md#unconfined-plugins-experimental),
executables an owner permits. Their profiles name a built-in dialect. A plugin
never defines a dialect, so adding a new dialect or lifecycle still requires its
own codec/runner and scoped behavioral evidence.

First-party contract references consulted for these compositions:
[OpenAI API migration](https://developers.openai.com/api/docs/guides/migrate-to-responses),
[Azure API lifecycle](https://learn.microsoft.com/en-us/azure/foundry/openai/api-version-lifecycle),
[Anthropic versions](https://platform.claude.com/docs/en/api/versioning),
[Claude on Vertex](https://platform.claude.com/docs/en/build-with-claude/claude-on-vertex-ai),
[Bedrock Converse](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html),
[Bedrock Invoke](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html).

A gateway using `OLP_CONNECTOR_CONFIG_FILE` without database decryption keys may
supply `network_credential_file` beside `credential_file` in each mounted
provider entry. A plugin provider mounts its static credential the same way;
its profile comes from the plugin its published revision pins. The network file
contains the same private JSON shape as the creation API. Its configured
credential UUID must match the published provider's network reference; mounting
a file cannot create authority. For an explicit profile the mounted
configuration must match the published model-significant and network settings;
only secret material is supplied locally. Current revocation still applies, and
mounted secret values are excluded from serialization and diagnostic formatting.

### Credential destination changes

Stored static credentials belong to a provider's authentication and destination
boundary. While a provider owns any unrevoked credential version, editing or restoring its
kind, authentication mode, endpoint, cloud context, profile, credential headers,
proxy or TLS trust roots requires a new provider with explicitly supplied
credentials, or revocation of all existing versions before supplying new ones. Detaching a slot does not release this restriction because old
versions can be selected again. Model, quota and connection timeout changes
remain available. Vertex service-account and application-default credentials use
Google's endpoint for the configured location and the system TLS trust roots.

Grant enrollment records both the plugin digest and profile ID. Changing either
requires re-enrollment before the draft can be probed, validated or activated;
existing published revisions retain their historical serving configuration.
This transition preserves the remaining credential boundary, including the
endpoint and plugin options. Changing those still requires revoking old
credentials or creating a new provider, even when changing the profile too.
Unexpired retained resources keep their grants refreshable under the original
provider revision until those resources expire or are deleted. Explicit
credential revocation still takes effect. Grants created before profile
provenance was recorded require re-enrollment; the migration does not infer
their original profile from mutable drafts.
