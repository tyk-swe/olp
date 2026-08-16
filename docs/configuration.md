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
| `OLP_PUBLIC_ORIGIN` | `http://127.0.0.1:8080` | OIDC redirects and generated links. |
| `OLP_LOCAL_LOGIN_ENABLED` | `true` | Keep local sign-in available after setup. |
| `OLP_TRUSTED_PROXY_CIDRS` | empty | Proxies allowed to supply `X-Forwarded-For`. |
| `OLP_CONSOLE_DIR` | `console/build` | Static console directory. |
| `OLP_MEDIA_SPOOL_DIR` | unset | On-disk media spool. |
| `OLP_MEDIA_SPOOL_CAPACITY_BYTES` | `1073741824` | Spool capacity (1 GiB). |
| `OLP_CONNECTOR_CONFIG_FILE` | unset | Optional file-backed connector mapping. |
| `RUST_LOG` | unset | Tracing filter, for example `olp=info,tower_http=info`. |

The CLI loopback default is intentional; Compose and Helm set their container
listener explicitly. Keep the observability listener private and set trusted
proxy CIDRs only to peers that append a trustworthy forwarding chain.

All HTTP modes (`all`, `gateway`, and `control`) require PostgreSQL and the
authentication HMAC key. Configure Valkey for production gateway traffic:
without it, runtime hints fall back to PostgreSQL polling and keys with hard
limits fail closed. The master key is required wherever database-managed
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

`.env.example` also defines `OLP_HOST_PORT`, `POSTGRES_PASSWORD`,
`POSTGRES_PASSWORD_URL_ENCODED`, `OLP_UID`, and `OLP_GID`. They configure the
Compose wrapper, not the binary. The encoded password is used in the database
URL; PostgreSQL receives the raw password.

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
run HTTPS, public-egress, SSRF, and reachability checks; only live exact-tuple
certification makes a capability eligible for activation. Use **Custom
endpoint** for another Bearer-key-compatible service.

## Test and harness variables

Never set the test escape hatches in production; these require the exact value
`test-only`:

- `OLP_ALLOW_INSECURE_OIDC_FOR_TESTS` permits HTTP OIDC issuers.
- `OLP_ALLOW_PARTIAL_MIGRATIONS_FOR_TESTS` builds N-1 migration fixtures.
- `OLP_ALLOW_INSECURE_PROVIDER_ENDPOINTS_FOR_TESTS` permits loopback mock
  provider endpoints. It is available only in debug binaries built with the
  `test-util` feature and is compiled out of release binaries.

Script and harness families are intentionally not runtime settings:
`OLP_TEST_DATABASE_*`, optional `OLP_VALKEY_URL`, and `OLP_CONSOLE_E2E_*`
support local suites; `OLP_E2E_*` supports the HA contract harness;
`OLP_REHEARSAL_*`, `OLP_BACKUP_*`, `OLP_RESTORE_*`, `OLP_PG_*`, and `OLP_PSQL`
support operations scripts; `OLP_SDK_SMOKE_*` supports SDK smoke; and
`OLP_LIVE_*`, `OLP_VERTEX_LIVE_*`, `OLP_AZURE_OPENAI_LIVE_*`, and
`OLP_BEDROCK_LIVE_*` opt into live-provider tests. See
[`CONTRIBUTING.md`](../CONTRIBUTING.md) and [`docs/operations.md`](operations.md)
for command-specific requirements.
