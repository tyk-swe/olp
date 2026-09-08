# Compose secrets

File-backed secrets for the local Compose stack: generation, bootstrap-token
retirement, key rotation, and optional connector configuration.

## Generate secrets

From the repository root:

```sh
./scripts/prepare-compose-secrets.sh
```

The helper creates only missing files, preserves operator material, applies
restrictive permissions, and never copies secrets into the image. Compose runs
as `1000:1000`; set `OLP_UID` and `OLP_GID` when the host user differs.

3.0 uses a JSON master-key ring and a separate authentication HMAC key. Provision
new 3.0 storage independently of 2.x. Preserve these files when restoring a 3.0
backup or rotating keys within an installation.

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
`olp master-key verify-retirement --version 1`. Remove the old key only after
retirement verification succeeds.

## File-backed connectors

Console-managed providers use encrypted credentials and do not set
`OLP_CONNECTOR_CONFIG_FILE`. A custom deployment may point that variable at a
read-only JSON file; every `provider_id` must match the active runtime. Start
from [`deploy/connectors.example.json`](../connectors.example.json); unknown
fields are rejected.

The top-level `providers` array contains entries with `provider_id`, a nested
`configuration` matching the management API, and an optional `credential_file`.
Vertex entries also select a probe `model`. Authentication mode is explicit:
Vertex `adc` and Bedrock `default_chain` omit stored credentials;
`service_account` and `static` require them. Separate vendor arrays are not
accepted by 3.0. See the
[mounted connector reference](../../docs/configuration.md#mounted-connectors).

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
