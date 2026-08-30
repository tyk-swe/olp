# Configuration reference

Application configuration is environment-driven; each application setting
also has a CLI flag in `olp <subcommand> --help`. Logging uses `RUST_LOG`. The
source of truth is `apps/olp/src/bootstrap/cli/config.rs`. Secrets are file
paths, never inline values.

## Runtime variables

| Variable | Default | Purpose |
|---|---|---|
| `OLP_DATABASE_URL` | required | PostgreSQL URL. |
| `OLP_DATABASE_MAX_CONNECTIONS` | `20` | Pool size. |
| `OLP_VALKEY_URL` | optional for `all`, `gateway`, `control`; required for `worker`, `migrate`, `doctor` | Valkey for installation-scoped limits, hints, and streams. |
| `OLP_LISTEN_ADDR` | `127.0.0.1:8080` | Public listener; containers override to `0.0.0.0:8080`. |
| `OLP_OBSERVABILITY_LISTEN_ADDR` | `127.0.0.1:9090` | Private health and metrics listener. |
| `OLP_HTTP_MAX_CONNECTIONS` | `1024` | Admitted TCP connections. |
| `OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS` | `256` | Inference work admission. |
| `OLP_HTTP_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS` | `32` | Management work admission. |
| `OLP_HTTP_CONNECTION_MAX_AGE_SECONDS` | `300` | Age at which HTTP/2 connections receive GOAWAY (1–86400). |
| `OLP_HTTP_CONNECTION_DRAIN_TIMEOUT_SECONDS` | `30` | Grace period for draining connections (1–3600). |
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
| `OLP_PROVIDER_MAX_RESPONSE_BYTES` | `16777216` | Largest provider response body buffered for non-streaming operations (1 MiB–256 MiB). |
| `OLP_PROVIDER_MAX_EVENT_BYTES` | `1048576` | Largest single streamed provider event (64 KiB up to the response cap). |
| `OLP_CONSOLE_DIR` | `console/build` | Static console directory. |
| `OLP_MEDIA_SPOOL_DIR` | unset | On-disk media spool. |
| `OLP_MEDIA_SPOOL_CAPACITY_BYTES` | `1073741824` | Spool capacity (1 GiB). |
| `OLP_CONNECTOR_CONFIG_FILE` | unset | Optional file-backed connector mapping. |
| `RUST_LOG` | unset | Tracing filter, for example `olp=info,tower_http=info`. |

The connection age only applies to HTTP/2 connections so clients rebalance
across replicas; HTTP/1 connections serve one response and are never cut short.
A draining connection is never force-closed while a response body is still
streaming: the drain timeout is re-armed until the stream ends, up to a fixed
one-hour ceiling. Keep the drain timeout below the Helm
`terminationGracePeriodSeconds − preStopDelaySeconds` budget, otherwise the
kubelet kills the pod before the listener finishes draining.

The CLI loopback default is intentional; Compose and Helm set their container
listener explicitly. Keep the observability listener private and set trusted
proxy CIDRs only to peers that append a trustworthy forwarding chain.

All HTTP modes (`all`, `gateway`, and `control`) require PostgreSQL and the
authentication HMAC key. Configure Valkey for production gateway traffic:
without it, runtime hints fall back to PostgreSQL polling and keys with hard
limits fail closed. When Valkey is configured but unreachable, the
`limits.valkey_unavailable` installation setting decides what hard-limited keys
get: `fail_closed` (default) rejects them with `503 distributed_limits_unavailable`;
`fail_open` admits them without rate limits, logs a warning per request, and
counts `olp_limits_fail_open_total`. Gateways poll the setting every 15 seconds
and load it once before binding the listener; an unconfigured Valkey never
fails open, and over-limit rejections are unaffected. The master key is
required wherever database-managed
provider or OIDC credentials and encrypted management replays are used.
`worker`, `migrate`, and `doctor` require both backing services but publish no
public listener. CLI flags override environment values. CLI-required settings
fail during startup before a listener is bound; omitting the master key instead
leaves database-encrypted runtime activation and control-plane operations
unavailable.

## File-based secrets

| Variable | Required by | Purpose |
|---|---|---|
| `OLP_MASTER_KEY_FILE` | `all`, `control`, a `gateway` loading database-encrypted credentials, `doctor`, `master-key` | Versioned envelope-encryption keyring. |
| `OLP_AUTH_HMAC_KEY_FILE` | `all`, `gateway`, `control`, `doctor` | Session and authentication HMAC key. |
| `OLP_BOOTSTRAP_TOKEN_FILE` | first `all` or `control` run | One-time owner-setup token. |

Generate and rotate these through
[`deploy/secrets/README.md`](../deploy/secrets/README.md). Keep the HMAC key
bytes when migrating from the old filename; replacing them invalidates stored
API-key and bootstrap-token digests.

## Compose-only variables

Compose accepts `OLP_IMAGE`, defaulting to the versioned release image used by
the quick start. `.env.example` also defines `OLP_HOST_PORT`,
`POSTGRES_PASSWORD`, `POSTGRES_PASSWORD_URL_ENCODED`, `OLP_UID`, and `OLP_GID`.
They configure the Compose wrapper, not the binary. The encoded password is
used in the database URL; PostgreSQL receives the raw password.

## OpenAI-compatible provider presets

The release-owned wizard catalog resolves a reviewed HTTPS endpoint and
`api_key` authentication into ordinary `openai_compatible` fields. The record
stores resolved values, not a catalog reference:

| ID | Provider | Endpoint |
|---|---|---|
| `groq` | Groq | `https://api.groq.com/openai/v1` |
| `mistral_ai` | Mistral AI | `https://api.mistral.ai/v1` |
| `together_ai` | Together AI | `https://api.together.ai/v1` |
| `xai` | xAI | `https://api.x.ai/v1` |
| `cerebras` | Cerebras | `https://api.cerebras.ai/v1` |
| `openrouter` | OpenRouter | `https://openrouter.ai/api/v1` |

A preset is not provider or model certification. Creation and edits still
run HTTPS, public-egress, SSRF, and reachability checks unless the host or
address is exempted by the egress allowlists below; only live exact-tuple
certification makes a capability eligible for activation. Use **Custom
endpoint** for another Bearer-key-compatible service.

## Body size caps

The JSON, media, and inline-media caps are validated together at startup:
an inline item must fit inside the inline total, the inline total must not
exceed the JSON cap, and the media cap must not exceed half of
`OLP_MEDIA_SPOOL_CAPACITY_BYTES`. Multipart admission budgets half the spool
for untrusted parsers, so a larger media cap would make every multipart
request fail with `503`. Raise the spool capacity (and its volume) before
raising the media cap. Per-endpoint multipart reservations scale with the
media cap: image edits reserve the full cap, image variations 55/64,
transcriptions 30/64, and video creation 25/64 of it. Header count and size
caps stay fixed.

The provider response caps apply to OpenAI-compatible, Anthropic, Gemini,
Azure OpenAI, and Vertex AI connectors; Bedrock speaks the AWS SDK and has no
byte cap. The response cap also bounds the events buffered while collecting
a non-streaming generation.

## Provider egress policy

Provider endpoints must use HTTPS and resolve only to public addresses:
literal hosts are checked before DNS, and every address in each DNS answer is
checked again before a pinned client is built, on every revalidation. Two
allowlists widen that policy for private or on-premises upstreams such as a
VPC-hosted vLLM server or an Azure private endpoint. Both default to empty,
which keeps the public-only behaviour.

- `OLP_PROVIDER_EGRESS_ALLOW_CIDRS` lists CIDRs (for example
  `10.0.0.0/8,fd00::/8`) exempt from the non-public denylist. The exemption
  applies to literal IP hosts and to every resolved address; an answer set
  that mixes allowlisted and denied addresses still fails closed, as does a
  later rebind outside the allowlist.
- `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS` lists exact hostnames or IP literals
  (lowercase, for example `vllm.internal,10.1.2.3`) whose endpoints may use
  `http://`. The scheme check is host-keyed because it runs synchronously,
  before DNS, on every management write.

A plain-HTTP endpoint on a private literal address needs both lists: the host
in the HTTP allowlist and the address inside an allowed CIDR. Both `serve`
modes and `doctor` accept the settings; startup logs a warning whenever either
list is non-empty. The allowlists never relax OIDC issuer or Vertex token
endpoint checks.

## Test and harness variables

Never set the test escape hatches in production; these require the exact value
`test-only`:

- `OLP_ALLOW_INSECURE_OIDC_FOR_TESTS` permits HTTP OIDC issuers.
- `OLP_ALLOW_PARTIAL_MIGRATIONS_FOR_TESTS` builds N-1 migration fixtures.

The e2e and console integration harnesses point providers at loopback mock
upstreams through the ordinary egress allowlists
(`OLP_PROVIDER_EGRESS_ALLOW_CIDRS=127.0.0.0/8,::1/128` and
`OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS=127.0.0.1,localhost`) rather than a
compiled-in escape hatch.

Script and harness families are intentionally not runtime settings:
`OLP_TEST_DATABASE_*`, optional `OLP_VALKEY_URL`, and `OLP_CONSOLE_E2E_*`
support local suites; `OLP_E2E_*` supports the HA contract harness;
`OLP_REHEARSAL_*`, `OLP_BACKUP_*`, `OLP_RESTORE_*`, `OLP_PG_*`, and `OLP_PSQL`
support operations scripts; `OLP_SDK_SMOKE_*` supports SDK smoke; and
`OLP_LIVE_*`, `OLP_VERTEX_LIVE_*`, `OLP_AZURE_OPENAI_LIVE_*`, and
`OLP_BEDROCK_LIVE_*` opt into live-provider tests. See
[`CONTRIBUTING.md`](../CONTRIBUTING.md) and [`docs/operations.md`](operations.md)
for command-specific requirements.
