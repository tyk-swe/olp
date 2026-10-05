# Configuration reference

Runtime settings come from environment variables or CLI flags; flags take
precedence. Use `olp <subcommand> --help` and the source in
[`internal/config/config.go`](../internal/config/config.go) for accepted flags.
Credential secrets use mounted files; database and Valkey URLs support either
inline or file-based settings. Invalid configuration fails before listeners
bind.

## Runtime variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `OLP_DATABASE_URL` | required | PostgreSQL URL. |
| `OLP_DATABASE_MAX_CONNECTIONS` | `20` | Pool size (1–10000), excluding detached worker sessions; see [connection budget](deployment.md#production-example-and-connection-budget). |
| `OLP_DATABASE_URL_FILE`, `OLP_VALKEY_URL_FILE` | unset | Read the corresponding URL from a mounted file; mutually exclusive with its inline setting. |
| `OLP_VALKEY_TLS_CA_FILE` | unset | PEM trust roots for Valkey TLS; requires `OLP_VALKEY_URL`. |
| `OLP_VALKEY_URL` | optional for `all`, `gateway`, `control`; required for `worker` | Valkey for installation-scoped limits, hints, and streams. |
| `OLP_LISTEN_ADDR` | `127.0.0.1:8080` | Public listener; containers override to `0.0.0.0:8080`. |
| `OLP_OBSERVABILITY_LISTEN_ADDR` | `127.0.0.1:9090` | Private health and metrics listener. |
| `OLP_OTLP_TRACES_ENDPOINT` | unset | Complete HTTP or HTTPS OTLP traces endpoint. Unset disables tracing. |
| `OLP_OTLP_HEADERS_FILE` | unset | JSON object of additional OTLP exporter headers, read only when tracing is enabled. |
| `OLP_TRACE_SAMPLE_RATIO` | `1.0` | Sampling ratio from `0.0` through `1.0` for locally rooted traces. |
| `OLP_TRACE_PROPAGATE_UPSTREAM` | `true` | Inject the current W3C trace context into provider attempts. |
| `OLP_TRACE_ACCEPT_INBOUND` | `true` | Accept a valid inbound W3C trace context as the request parent. |
| `OLP_HTTP_MAX_CONNECTIONS` | `1024` | Admitted TCP connections. |
| `OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS` | `256` | Inference work admission (1–100000); excess work receives 503. |
| `OLP_HTTP_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS` | `32` | Management and console work admission (1–100000). |
| `OLP_HTTP_CONNECTION_MAX_AGE_SECONDS` | `300` | Age at which HTTP/2 connections receive GOAWAY (1–86400). |
| `OLP_HTTP_CONNECTION_DRAIN_TIMEOUT_SECONDS` | `30` | Grace period for draining connections (1–600). |
| `OLP_PUBLIC_ORIGIN` | `http://127.0.0.1:8080` | OIDC redirects and generated links. |
| `OLP_LOCAL_LOGIN_ENABLED` | `true` | Keep local sign-in available after setup. |
| `OLP_TRUSTED_PROXY_CIDRS` | empty | Proxies allowed to supply `X-Forwarded-For`. |
| `OLP_GATEWAY_CORS_ALLOWED_ORIGINS` | empty | Browser origins allowed to call the inference gateway cross-origin; wildcards are refused and the management API stays same-origin. |
| `OLP_PROVIDER_EGRESS_ALLOW_CIDRS` | empty | CIDRs exempt from the non-public provider egress denylist; see [Provider egress policy](#provider-egress-policy). |
| `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS` | empty | Hostnames or IP literals whose provider endpoints may use plain HTTP. |
| `OLP_HTTP_MAX_JSON_BODY_BYTES` | `2097152` | Largest JSON request body, before and after gzip inflation (64 KiB–64 MiB). |
| `OLP_HTTP_MAX_MEDIA_BODY_BYTES` | `67108864` | Largest raw or multipart media request body (1 MiB–1 GiB); see [Body size caps](#body-size-caps). |
| `OLP_HTTP_MAX_INLINE_MEDIA_ITEMS` | `4` | Inline base64 media items accepted per JSON request (1–64). |
| `OLP_HTTP_MAX_INLINE_MEDIA_ITEM_BYTES` | `1048576` | Decoded cap for one inline media item (1 KiB–64 MiB). |
| `OLP_HTTP_MAX_INLINE_MEDIA_TOTAL_BYTES` | `2097152` | Decoded cap for all inline media in one request (1 KiB–64 MiB). |
| `OLP_PROVIDER_MAX_RESPONSE_BYTES` | `16777216` | Largest provider response body buffered for non-streaming operations, including a stream aggregated for a non-streaming caller (1 MiB–256 MiB). |
| `OLP_PROVIDER_MAX_EVENT_BYTES` | `1048576` | Largest single streamed provider event (64 KiB up to the response cap). |
| `OLP_CONSOLE_DIR` | `console/build` | Static console directory. |
| `OLP_MEDIA_SPOOL_DIR` | unset | On-disk media spool; defaults to the system temp directory. |
| `OLP_MEDIA_SPOOL_CAPACITY_BYTES` | `1073741824` | Spool capacity (1 GiB; at least 256 MiB). |
| `OLP_CONNECTOR_CONFIG_FILE` | unset | Optional file-backed connector mapping. |
| `OLP_UNCONFINED_PLUGIN_DIR` | unset | Experimental. Absolute directory of the image that holds unconfined plugin executables. Setting it enables the [unconfined plugin tier](plugins.md#unconfined-plugins-experimental); nothing else can. |
| `OLP_LOG_LEVEL` | `info` | JSON log severity: debug, info, warn, error. |
| `OLP_SHUTDOWN_TIMEOUT` | `30s` | Shared HTTP, metadata, delivery and worker shutdown budget (1ms–10m). |
| `OLP_DEPENDENCY_REQUEST_TIMEOUT` | `2s` | Per-request dependency deadline (1ms–1m). |
| `OLP_STARTUP_TIMEOUT` | `10s` | Startup and ordinary maintenance deadline (1ms–1m). |

At maximum connection age the server stops admitting requests on that connection
and sends HTTP/2 GOAWAY. Existing streams have the configured drain interval to
finish; expiration closes the connection. SIGTERM stops admission and gives
HTTP, metadata delivery and workers one shared shutdown budget. Keep
`OLP_SHUTDOWN_TIMEOUT` below the deployment termination grace minus its pre-stop
delay. Forced termination can leave accounting completeness gaps.

The CLI loopback default is intentional; Compose and Helm set their container
listener explicitly. Keep the observability listener private and set trusted
proxy CIDRs only to peers that append a trustworthy forwarding chain.

Tracing is enabled only when `OLP_OTLP_TRACES_ENDPOINT` is set. Supply the full
URL, such as `https://collector.example.com/v1/traces`; OLP does not rewrite its
path. Endpoint userinfo and fragments are rejected. Tracing exports spans only;
Prometheus metrics and JSON logs keep their existing destinations.

Exporter credentials belong in `OLP_OTLP_HEADERS_FILE`, a UTF-8 JSON object of
valid HTTP header names and values, for example `{"x-scope-orgid":"tenant-a"}`.
The file uses the secret permissions below. Inline
`OTEL_EXPORTER_OTLP_TRACES_HEADERS` and `OTEL_EXPORTER_OTLP_HEADERS` are
rejected when tracing is enabled. Invalid endpoints, ratios, or header files
fail startup.

Inbound `traceparent` is accepted only when tracing and inbound acceptance are
enabled; invalid context starts a local trace. Caller `tracestate` is discarded.
Upstream propagation uses fresh span headers, never exporter credentials. Spans
contain only allowed identifiers, classification, timing, usage, and pricing,
without prompts, outputs, tool data, raw headers, or provider errors. Only
canonical lowercase hyphenated UUID `x-request-id` values enter the trace
attribute. See [tracing operations](operations.md#distributed-tracing) for
sampling, monitoring, and local exploration.

All HTTP modes (`all`, `gateway`, `control`) require PostgreSQL and the
authentication HMAC key. Configure Valkey for production: without it, runtime
hints fall back to polling and hard-limited keys fail closed. `worker` requires
Valkey and exposes only private health/metrics; `migrate` and `doctor` expose no
listener. Doctor checks Valkey when configured. See
[process modes](gateway.md#process-modes) for worker and connector requirements.

`limits.valkey_unavailable` is a database-managed installation setting. Its
`fail_closed` default rejects unenforceable limits; `fail_open` can bypass
rate/concurrency-only key limits during a configured Valkey outage. Key and
group cost budgets always fail closed. See
[limit enforcement](gateway.md#limits-and-budgets) for polling, error codes, and
provider quotas.

## File-based secrets

Use regular files with mode `0400`, `0440`, `0600`, or `0640`; group-write and
world permissions are rejected. A configured bootstrap file may be absent after
setup. Workers require both the HMAC and master keys, including with mounted
connectors. Every command refuses to start when `OLP_AUTH_HMAC_KEY`,
`OLP_MASTER_KEY`, or `OLP_BOOTSTRAP_TOKEN` is set inline.

| Variable | Required by | File contents |
| --- | --- | --- |
| `OLP_MASTER_KEY_FILE` | `all`, `control`, `worker`, a `gateway` loading database-encrypted credentials, `doctor`, `master-key` | JSON master-key ring shown below. |
| `OLP_AUTH_HMAC_KEY_FILE` | `all`, `gateway`, `control`, `worker`, `doctor`, `master-key` | 32 random bytes, encoded as hex or standard base64. |
| `OLP_BOOTSTRAP_TOKEN_FILE` | first `all` or `control` run | Random 32–256-byte token for one-time owner setup. |
| `OLP_OTLP_HEADERS_FILE` | traced `all`, `gateway`, `control`, or `worker` | Optional JSON object of OTLP exporter headers. |

```json
{
  "active_version": 1,
  "keys": [{ "version": 1, "key": "<32 random bytes encoded as hex or base64>" }]
}
```

A ring holds 1–32 distinct positive versions. Keep the authentication HMAC key
stable: its installation fingerprint prevents accidental replacement, and
replacing it invalidates stored API-key and bootstrap-token digests. Preserve
both files for recovery; follow the
[key rotation procedure](operations.md#master-key-rotation-and-recovery)
before retiring any master-key version.

Generate Compose files with the [secret helper](../deploy/secrets/README.md).
For local/test scripts, `source scripts/secrets.sh <private-directory>` creates
missing secret files without printing their contents. Production secret
distribution remains the operator's responsibility. Database and Valkey URLs
also accept `_FILE` settings, mutually exclusive with their inline values.

## Compose-only variables

Compose accepts `OLP_IMAGE`, defaulting to the versioned release image used by
the quick start. `.env.example` also defines `OLP_HOST_PORT`,
`POSTGRES_PASSWORD`, `POSTGRES_PASSWORD_URL_ENCODED`, `OLP_UID`, and `OLP_GID`.
`OLP_COMPOSE_SECRETS_DIR` selects an absolute secret-directory override for
Compose and its preparation/retirement helpers. These settings configure the
wrapper rather than the binary. The encoded password is used in the database
URL; PostgreSQL receives the raw password. The tracing overlay
`deploy/compose.tracing.yaml` reads `OLP_TRACE_SAMPLE_RATIO`,
`OLP_TRACE_PROPAGATE_UPSTREAM`, and `OLP_TRACE_ACCEPT_INBOUND` from the same
file; leave them empty to keep the binary defaults.

## OpenAI-compatible provider presets

The release-owned wizard catalog resolves a reviewed HTTPS endpoint and
`api_key` authentication into ordinary `openai_compatible` fields. Self-hosted
runtimes (`vllm`, `ollama`, `lmstudio`, `llamacpp`, `docker-model-runner`) are
the exception: they start unauthenticated, with a placeholder endpoint to
replace with the operator's server. Account-scoped platforms (`databricks`,
`snowflake-cortex`, `cloudflare-workers-ai`) also start at a placeholder whose
host or path names the operator's account. The record stores the vendor ID
separately from its editable resolved connection values.

Each preset is a reviewed vendor contract in
[`internal/vendors`](../internal/vendors/catalog.go): its operations, generation
dialects, the request fields its documentation refuses or renames, and the
documentation it was reviewed against. Its evidence, under
[`tests/fixtures/vendors`](../tests/fixtures/vendors), pins the documented
endpoints, refused parameters, usage shape, stream termination and error
envelope, and states for each fixture whether it is a documented example or
follows the documented schema or the OpenAI dialect where the vendor's
documentation is silent. A preset whose vendor speaks Chat Completions exactly
selects the `compatible-chat` profile, so it can serve strict routes; choose
**Automatic** to use parameter defaults instead.

| ID | Provider | Endpoint | Profile |
| --- | --- | --- | --- |
| `groq` | Groq | `https://api.groq.com/openai/v1` | `compatible-chat` |
| `mistral` | Mistral | `https://api.mistral.ai/v1` |  |
| `openrouter` | OpenRouter | `https://openrouter.ai/api/v1` | `compatible-chat` |
| `together` | Together AI | `https://api.together.ai/v1` |  |
| `vllm` | vLLM | `https://vllm.example.internal/v1` (placeholder) |  |
| `deepseek` | DeepSeek | `https://api.deepseek.com` |  |
| `fireworks` | Fireworks | `https://api.fireworks.ai/inference/v1` | `compatible-chat` |
| `deepinfra` | DeepInfra | `https://api.deepinfra.com/v1/openai` |  |
| `huggingface` | Hugging Face | `https://router.huggingface.co/v1` |  |
| `cohere` | Cohere | `https://api.cohere.ai/compatibility/v1` |  |
| `cohere-native-v2` | Cohere native v2 | `https://api.cohere.ai/v2` | `cohere-v2` |
| `voyage` | Voyage AI | `https://api.voyageai.com/v1` |  |
| `xai` | xAI | `https://api.x.ai/v1` | `compatible-chat` |
| `cerebras` | Cerebras | `https://api.cerebras.ai/v1` | `compatible-chat` |
| `sambanova` | SambaNova | `https://api.sambanova.ai/v1` | `compatible-chat` |
| `nebius` | Nebius Token Factory | `https://api.tokenfactory.nebius.com/v1` | `compatible-chat` |
| `novita` | Novita AI | `https://api.novita.ai/openai/v1` |  |
| `nvidia-nim` | NVIDIA NIM | `https://integrate.api.nvidia.com/v1` |  |
| `featherless` | Featherless | `https://api.featherless.ai/v1` |  |
| `baseten` | Baseten | `https://inference.baseten.co/v1` |  |
| `moonshot` | Moonshot AI | `https://api.moonshot.ai/v1` | `compatible-chat` |
| `moonshot-cn` | Moonshot AI (China) | `https://api.moonshot.cn/v1` | `compatible-chat` |
| `dashscope` | Alibaba Cloud Model Studio | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` | `compatible-chat` |
| `dashscope-cn` | Alibaba Cloud Model Studio (China) | `https://dashscope.aliyuncs.com/compatible-mode/v1` | `compatible-chat` |
| `zai` | Z.ai | `https://api.z.ai/api/paas/v4` |  |
| `zhipu` | Zhipu BigModel | `https://open.bigmodel.cn/api/paas/v4` |  |
| `minimax` | MiniMax | `https://api.minimax.io/v1` | `compatible-chat` |
| `minimax-cn` | MiniMax (China) | `https://api.minimax.cn/v1` | `compatible-chat` |
| `volcengine-ark` | Volcengine Ark | `https://ark.cn-beijing.volces.com/api/v3` | `compatible-chat` |
| `byteplus-modelark` | BytePlus ModelArk | `https://ark.ap-southeast.bytepluses.com/api/v3` | `compatible-chat` |
| `scaleway` | Scaleway Generative APIs | `https://api.scaleway.ai/v1` | `compatible-chat` |
| `ovhcloud` | OVHcloud AI Endpoints | `https://oai.endpoints.kepler.ai.cloud.ovh.net/v1` | `compatible-chat` |
| `nscale` | Nscale | `https://inference.api.nscale.com/v1` | `compatible-chat` |
| `databricks` | Databricks | `https://your-workspace.cloud.databricks.com/ai-gateway/mlflow/v1` (placeholder) |  |
| `snowflake-cortex` | Snowflake Cortex | `https://your-account.snowflakecomputing.com/api/v2/cortex/v1` (placeholder) |  |
| `cloudflare-workers-ai` | Cloudflare Workers AI | `https://api.cloudflare.com/client/v4/accounts/your-account-id/ai/v1` (placeholder) |  |
| `vercel-ai-gateway` | Vercel AI Gateway | `https://ai-gateway.vercel.sh/v1` |  |
| `ollama` | Ollama | `https://ollama.example.internal/v1` (placeholder) |  |
| `lmstudio` | LM Studio | `https://lmstudio.example.internal/v1` (placeholder) |  |
| `llamacpp` | llama.cpp server | `https://llamacpp.example.internal/v1` (placeholder) |  |
| `docker-model-runner` | Docker Model Runner | `https://model-runner.example.internal/engines/v1` (placeholder) |  |

A preset is not provider or model certification. Creation and edits still run
HTTPS, public-egress, SSRF, and reachability checks unless the host or address
is exempted by the egress allowlists below; only live exact-tuple certification
makes a capability eligible for activation. Use **Custom endpoint** for another
compatible service, including explicitly configured private HTTP and
unauthenticated endpoints. See [provider routing](provider-routing.md) for
encrypted custom headers, defaults, model facts, credential pools, quotas, and
installation/route/key/request policies.

## Body size caps

The JSON, media, and inline-media caps are validated together at startup: an
inline item must fit inside the inline total, both stay within 64 MiB, and the
media cap must not exceed half of `OLP_MEDIA_SPOOL_CAPACITY_BYTES`. Multipart
admission budgets half the spool for untrusted parsers, so a larger media cap
would make every multipart request fail with `503`. Raise the spool capacity
(and its volume) before raising the media cap. Per-endpoint multipart
reservations scale with the media cap: image edits reserve the full cap, image
variations 55/64, transcriptions 30/64, and video creation 25/64 of it. Header
count and size caps stay fixed.

The provider response caps apply to OpenAI-compatible, Anthropic, Gemini, Azure
OpenAI, Vertex AI and Bedrock connectors. The response cap also bounds buffered
events when collecting a non-streaming generation.

## Provider egress policy

Provider endpoints must be absolute HTTPS URLs without credentials, query
strings, or fragments, and resolve only to public addresses: literal hosts are
checked before DNS, and every address in each DNS answer is checked again before
a pinned client is built, on every revalidation. Two allowlists widen that
policy for private or on-premises upstreams such as a VPC-hosted vLLM server or
an Azure private endpoint. Both default to empty, which keeps the public-only
behavior.

- `OLP_PROVIDER_EGRESS_ALLOW_CIDRS` lists CIDRs (for example
  `10.0.0.0/8,fd00::/8`) exempt from the non-public denylist. The exemption
  applies to literal IP hosts and to every resolved address; an answer set
  that mixes allowlisted and denied addresses still fails closed, as does a
  later rebind outside the allowlist.
- `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS` lists exact hostnames or IP literals
  (lowercase, for example `vllm.internal,10.1.2.3`) whose endpoints may use
  `http://`. The scheme check is host-keyed because it runs synchronously,
  before DNS, on every management write.

A plain-HTTP endpoint on a private literal address needs both lists: the host in
the HTTP allowlist and the address inside an allowed CIDR. The `all`, `gateway`,
`control`, and `doctor` commands accept the settings; `all`, `gateway`, and
`control` log a warning naming both lists at startup whenever either is
non-empty. Transports refuse redirects, use TLS 1.2 or
newer, bound dial and handshake timeouts, and cap response headers at 32 KiB.
Probes and inference use the same normalized endpoint and egress policy. The
allowlists never relax OIDC issuer or Vertex token endpoint checks.
These exceptions are installation-wide administrative trust decisions;
they do not provide origin-specific tenant isolation. See the
[egress](../internal/egress/) and [OIDC](../tests/integration/oidc_test.go) tests
for boundary coverage.

## Test and harness variables

Loopback OIDC is available only in an explicitly compiled `-tags=oidctest` test
binary; release binaries have no environment variable that relaxes OIDC
transport checks.

The e2e and console integration harnesses point providers at loopback mock
upstreams through the ordinary egress allowlists
(`OLP_PROVIDER_EGRESS_ALLOW_CIDRS=127.0.0.0/8,::1/128` and
`OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS=127.0.0.1,localhost`) rather than a
compiled-in escape hatch.

Script and harness families are intentionally not runtime settings:
`OLP_TEST_DATABASE_*`, optional `OLP_VALKEY_URL`, and `OLP_CONSOLE_E2E_*`
support local suites; `OLP_BACKUP_*`, `OLP_RESTORE_*`, and `OLP_PSQL` support
operations scripts; `OLP_SDK_SMOKE_*` supports SDK smoke; and `OLP_LIVE_*`,
`OLP_VERTEX_LIVE_*`, `OLP_AZURE_OPENAI_LIVE_*`, and `OLP_BEDROCK_LIVE_*` opt
into live-provider tests. See [`CONTRIBUTING.md`](../CONTRIBUTING.md) and
[`docs/operations.md`](operations.md) for command-specific requirements.

## Mounted connectors

`OLP_CONNECTOR_CONFIG_FILE` accepts the `providers` list shown in
[`deploy/connectors.example.json`](../deploy/connectors.example.json). Each
entry identifies a provider, uses the same nested `configuration` as the
management API, and references an optional `credential_file`. Vertex entries
also select a probe `model`. Credential files must have restricted permissions;
ADC and the AWS default chain reject stored credentials. A provider with a
profile, including a [plugin provider](plugins.md#providers-from-plugin-profiles)
and its static credential, mounts only secret material: its `configuration` must
match the published revision. A provider that authenticates with a
[grant](plugins.md#grant-enrollment) can't be mounted: its grant lives beneath a
credential version in the database, which needs the master key.

Without `OLP_MASTER_KEY_FILE`, each mounted connector serves the published
default credential slot and enforces its slot and connection limits. Releases
with enabled named slots require the master key so each attempt can use its
published credential version. Database-encrypted revisions remain authoritative
when the master key is configured.

Production Compose can generate database credentials and their encoded URL using
`scripts/prepare-compose-production.sh`; see [deployment.md](deployment.md).

## Configuration promotion artifacts

`GET /api/v1/configuration/export` returns a secret-free desired-state artifact
(`openllmproxy.dev/config/v1`) and its canonical digest: SHA-256 lowercase hex
of the canonical JSON with `exported_at` blanked and every collection sorted
deterministically. Projects, providers, routes, and targets are identified by
natural names — never UUIDs — and `project` carries a project name or `null` for
installation-wide resources. Export reads the active provider revision when a
provider is active and the current draft otherwise, the latest revision of every
published route (including retired routes, marked `retired: true`), and the
latest effective pricing revision. It never contains credential IDs, ciphertext,
API keys, users, management tokens, usage, audit, request or media data,
certification evidence, runtime IDs, or secret values. Slots that hold
credential authority export a stable `credential_ref` of
`provider-name/slot-name-or-default`; credentialless authentication modes export
`null`. A plugin provider's configuration references the plugin build it pins by
digest (`profile_revision`), never the module. Every slot of a provider that
authenticates with a [grant](plugins.md#grant-enrollment) exports its
`credential_ref`, enrolled or not, and no grant material: no tokens, observed
principal or grant facts. Slot `allowed_api_keys` restrictions are not portable
— API keys are installation-local — so export always emits an empty list and
import rejects a non-empty one; re-establish them on the destination after
creating keys. Apply keeps the restriction an existing destination slot already
has.

Every exported route carries an explicit
[fidelity](provider-routing.md#route-fidelity) mode. A route entry without
fidelity, or with `null` or `{}`, is strict; plan and apply never inherit the
destination's current mode, so an export applied elsewhere reproduces its routes
exactly. An invalid mode fails validation on `routes.N.fidelity`, and a strict
route with a `redact` content-policy rule fails the plan with
`fidelity_policy_conflict`. Configuration staging never activates a strict route
or bypasses its policy checks.

`POST /api/v1/configuration/plan` validates an artifact and reports
`{digest, actions, conflicts, blockers}` without mutating. Validation rejects
unknown fields, oversized collections, duplicate natural identities
(case-insensitive for projects and providers, exact for routes, models, and
credential references), cross-project targets, bindings for refs the artifact
does not declare, and secrets over 64 KiB. A plugin provider pins its plugin
profile as a saved draft does, and its endpoint follows from the profile's
address and the provider's plugin options. A plugin build the destination can't
use is a `plugin` blocker keyed by its digest: `plugin_not_installed` or
`plugin_not_approved` until an owner installs and approves that build, or
`plugin_unconfined_disabled` for an unconfined build while the deployment does
not enable unconfined plugins. Its providers are validated against the profile
once it is usable. A profile the build does not declare
(`plugin_profile_unknown`) refuses the artifact. Plan reports
`secret_binding_required` blockers for credential refs that do not already
resolve to a current same-named slot credential of the provider's
authentication on the destination, static plugin credentials included. A slot a
grant backs takes no secret binding: unless it already holds a grant that the
plugin build the artifact pins enrolled on the destination, plan reports its
ref as an `enroll` action
(`grant_enrollment_required`), and after applying the provider activates only
once a grant enrollment gives each slot it serves with a credential.

`POST /api/v1/configuration/apply` requires an Idempotency-Key and stages the
desired state in one installation-serialized transaction:

- Missing projects are created with the applying member (or the token's
  creator) as their manager; existing case-insensitive names are reused.
- Missing providers become drafts; existing providers get their draft
  fields, models, and slots replaced and `draft_dirty` set — an active
  revision is never mutated and keeps serving until local certification
  and activation. A provider kind change is a `provider_kind_changed`
  conflict. Imported capabilities are always stored `declared`, never
  `certified`.
- Routes become new or replaced non-activated drafts with their routing
  policy staged; apply never activates or retires. A published same-slug
  route in another project is a conflict; a lifecycle difference reports
  `route_lifecycle_requires_activation` or
  `route_lifecycle_requires_retirement`.
- Pricing is a noop when the artifact's prices exactly equal the latest
  revision; otherwise a new immutable revision is created and the runtime is
  republished. A future `effective_at` is preserved, while a past or current
  one is rebased to apply time (`effective_at_rebased`). Provider and route
  drafts never alter runtime.

`secret_bindings` maps `credential_ref` to the environment-specific secret. It
is write-only: never echoed in responses, audited, logged, or replayed in
plaintext — the replay fingerprint stores only the document digest plus sorted
binding names and SHA-256 digests of their values. `expected_digest` compares
against the destination's current export digest; a mismatch is a
`configuration_changed` conflict. Apply returns
`409 configuration_not_applicable` with the same plan body whenever conflicts or
blockers remain, and rolls back every write. A successful apply records one
`configuration.apply` audit event with the artifact digest as its resource.

All three endpoints require the `configure` operation and an all-projects
principal, so assigned users and project-scoped machine tokens receive 403;
all-project machine tokens with `read` and `configure` scopes can automate
export, plan, and apply. The console exposes the workflow to global
owner/operator sessions under **Settings → Configuration promotion**, which
asks for the secret bindings a plan requires and forgets them whenever the
artifact changes.

### Versioned provider profiles and per-connection networking

Provider drafts can select explicit API/hosting profiles, operation/dialect defaults,
semantic headers, serving bindings and secure per-connection proxy/TLS settings.
Network secrets use provider-owned encrypted references, independently from API
credential slots. See [provider profiles](provider-profiles.md) for the public
configuration shape, cloud endpoint differences, inheritance rules, network bounds,
credential lifecycle and qualification scope. A provider that omits the profile
fields is an Automatic provider, whose endpoints follow from its provider kind.
