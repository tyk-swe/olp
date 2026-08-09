# Compose secrets

File-backed secrets for the local Compose stack: generation, bootstrap-token
retirement, key rotation, and optional connector configuration.

## Generate and migrate secrets

From the repository root:

```sh
./scripts/prepare-compose-secrets.sh
```

The helper creates only missing files, preserves operator material, applies
restrictive permissions, and never copies secrets into the image. Compose runs
as `1000:1000`; set `OLP_UID` and `OLP_GID` when the host user differs.

Existing installations must preserve authentication HMAC bytes. Before an
upgrade, rename the legacy file without changing it:

```sh
mv deploy/secrets/olp_key_hash_key deploy/secrets/olp_auth_hmac_key
```

The helper refuses to generate the new file while the legacy name exists;
replacing it would invalidate persisted authentication digests. The complete
2.0 naming migration is in
[`docs/operations.md`](../../docs/operations.md#naming-migration-prerequisites).

## Bootstrap token lifecycle

`olp_bootstrap_token` is a one-time first-owner setup token. Start a new
installation with `deploy/compose.yaml` and
`deploy/compose.bootstrap.yaml`, paste its value into the setup form, and
verify the owner. Recreate the initialized application without the bootstrap
overlay, then retire the token:

```sh
docker compose --env-file .env -f deploy/compose.yaml up -d --force-recreate olp
./scripts/retire-compose-bootstrap-secret.sh
```

The helper deletes the token and records retirement so preparation cannot
recreate it. Use the base Compose file for all later restarts/upgrades. To
intentionally bootstrap a fresh database, remove
`deploy/secrets/.olp_bootstrap_retired`, prepare again, and include the overlay.

## Master-key rotation

Use a versioned keyring, retaining the old key until all encrypted rows are
rewritten and verified:

```json
{
  "active_version": 2,
  "keys": [
    { "version": 1, "key": "<old-base64-key>" },
    { "version": 2, "key": "<new-base64-key>" }
  ]
}
```

Add the new key and restart every replica, select it as active and restart
again, then run `olp master-key reencrypt` and
`olp master-key verify-retirement --version 1`. Follow the
[operations rotation procedure](../../docs/operations.md#master-key-rotation)
before removing a key.

## File-backed connectors

Console-managed providers use encrypted credentials and do not set
`OLP_CONNECTOR_CONFIG_FILE`. A custom deployment may point that variable at a
read-only JSON file; every `provider_id` must match the active runtime.

| Array key | Required fields |
|---|---|
| `openai` | `provider_id`, optional `base_url`, `credential_file` |
| `azure_openai` | `provider_id`, `endpoint`, `deployment`, `api_version`, `credential_file` |
| `vertex` | `provider_id`, `project`, `location`, `model`; `adc` omits `credential_file`, `service_account` requires it |
| `bedrock` | `provider_id`, `region`; `default_chain` omits `credential_file`, `static` requires it |

Mount the configuration and credential files read-only (`0600` for credentials).
Prefer workload identity. A static Bedrock file is:

```json
{
  "access_key_id": "AKIA...",
  "secret_access_key": "...",
  "session_token": "<optional>"
}
```

Bedrock discovery needs `bedrock:ListFoundationModels`; inference needs
`bedrock:InvokeModel`, `bedrock:InvokeModelWithResponseStream`, and, when
used, `bedrock:CountTokens`. Scope permissions to configured resources where
AWS supports resource-level grants.
