# Compose secrets

This directory holds the file-backed secrets consumed by the local Compose
stack. This document covers secret generation, the bootstrap-token lifecycle,
production key rotation, and file-backed connector configuration.

## Generating local secrets

Create local Compose keys from the repository root. The helper creates only
missing files, preserves operator-supplied material, and sets restrictive file
permissions:

```sh
./scripts/prepare-compose-secrets.sh
```

The generated files are ignored by Git and are not copied into the application
image. Compose runs OpenLLMProxy as `1000:1000` by default; set `OLP_UID` and
`OLP_GID` to the host user's IDs when necessary.

Existing installations must preserve the authentication HMAC key bytes. Before
running the helper after an upgrade, rename the legacy file without changing its
contents:

```sh
mv deploy/secrets/olp_key_hash_key deploy/secrets/olp_auth_hmac_key
```

The helper refuses to generate `olp_auth_hmac_key` while the legacy filename is
present, because replacing this key would invalidate persisted authentication
digests.

## Bootstrap token lifecycle

`olp_bootstrap_token` is a one-time first-owner setup token. A new installation
must include `deploy/compose.bootstrap.yaml` alongside the base Compose file.
Paste the value into the console's setup form and verify the owner account.

Before deleting the token, recreate the initialized application without the
bootstrap overlay. Retire it with the helper only after that succeeds:

```sh
docker compose --env-file .env -f deploy/compose.yaml up -d --force-recreate olp
./scripts/retire-compose-bootstrap-secret.sh
```

The retirement helper deletes `olp_bootstrap_token` and records its retirement
so `prepare-compose-secrets.sh` will not recreate a meaningless replacement.
Use `deploy/compose.yaml` alone for all later restarts and upgrades. To abandon
the initialized database and intentionally bootstrap a fresh installation,
remove `deploy/secrets/.olp_bootstrap_retired`, rerun the preparation helper,
and add the bootstrap overlay again.

## Production key rotation

A single base64 master key is version 1. Rotation uses a versioned keyring; this
example shows version 2 after activation:

```json
{
  "active_version": 2,
  "keys": [
    { "version": 1, "key": "<old-base64-key>" },
    { "version": 2, "key": "<new-base64-key>" }
  ]
}
```

Add the new key while the old version is still active, restart every replica,
then select the new active version and restart again. Retain the old key until
`olp master-key reencrypt` has rewritten all encrypted rows. Run
`olp master-key verify-retirement --version 1` to confirm that no references
remain. Follow the complete
[rotation procedure](../../docs/operations.md#master-key-rotation)
before removing a key.

## File-backed connector configuration

The stock Compose stack and Helm chart use console-managed providers and
encrypted credentials; they do not set `OLP_CONNECTOR_CONFIG_FILE`. A custom
deployment can set that variable to the path of a mounted JSON configuration
file when credentials must remain file-backed. See the
[connector configuration example](../connectors.example.json).

The JSON object supports these array keys and provider fields. Each
`provider_id` must match the corresponding provider in the active runtime
configuration.

| Key | Fields |
|---|---|
| `openai` | `provider_id`, optional `base_url`, and a required `credential_file` containing the API key |
| `azure_openai` | `provider_id`, `endpoint`, `deployment`, `api_version`, and a required `credential_file` containing the API key |
| `vertex` | `provider_id`, `project`, `location`, `model`, and optional `auth_mode`. The default `adc` mode must omit `credential_file`; `service_account` requires a credential file containing service-account JSON |
| `bedrock` | `provider_id`, `region`, and optional `auth_mode`. The default `default_chain` mode must omit `credential_file`; `static` requires a credential file containing static AWS credentials |

Mount the configuration and every referenced credential file read-only. Use
mode `0600` for credential files. Prefer workload identity (`adc` for Vertex AI
or `default_chain` with an IAM role for Amazon Bedrock); use
`service_account` or `static` only when workload identity is unavailable. A
static Bedrock credential file has this shape:

```json
{
  "access_key_id": "AKIA...",
  "secret_access_key": "...",
  "session_token": "<optional>"
}
```

Bedrock discovery requires `bedrock:ListFoundationModels`; inference requires
`bedrock:InvokeModel`, `bedrock:InvokeModelWithResponseStream`, and, when used,
`bedrock:CountTokens`. Scope runtime permissions to the configured resources
where AWS supports resource-level permissions.
