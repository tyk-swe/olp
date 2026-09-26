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

To store secrets elsewhere, set `OLP_COMPOSE_SECRETS_DIR` to an absolute path
for every preparation, retirement, and Docker Compose command. The helpers
reject relative overrides because shell commands and Compose files resolve
relative paths from different directories. When unset, the directory remains
`deploy/secrets`.

The master key is a JSON key ring, and authentication uses a separate HMAC key.
Preserve both files when restoring a backup or rotating keys within an
installation.

## Bootstrap token lifecycle

`olp_bootstrap_token` is a one-time first-owner setup token. Start a new
installation with `deploy/compose.yaml` and `deploy/compose.bootstrap.yaml`,
paste its value into the setup form, and verify the owner. Recreate the
initialized application without the bootstrap overlay, then retire the token.
Retain any build or production overlays and environment files used by your
installation; for the base stack:

```sh
docker compose --env-file .env -f deploy/compose.yaml up -d --force-recreate olp
./scripts/retire-compose-bootstrap-secret.sh
```

The helper deletes the token and records retirement so preparation cannot
recreate it. Omit the bootstrap overlay on later restarts. To
intentionally bootstrap a fresh database, remove
`deploy/secrets/.olp_bootstrap_retired`, prepare again, and include the overlay.

## Master-key rotation

Follow the
[master-key rotation procedure](../../docs/access.md#master-key-rotation-and-recovery)
in a controlled maintenance window. Retain every referenced key version and keep
the authentication HMAC key unchanged. Reencrypt and authenticate records before
removing an old version; the retirement command takes a positional version, for
example `olp master-key verify-retirement 1`. Backup retention may require
keeping that version longer.

## File-backed connectors

Console-managed providers use encrypted credentials and do not set
`OLP_CONNECTOR_CONFIG_FILE`. A custom deployment may point that variable at a
read-only JSON file; every `provider_id` must match the active runtime. Start
from [`deploy/connectors.example.json`](../connectors.example.json); unknown
fields are rejected.

Mount configuration and credential files read-only (`0600` for credentials). The
[mounted connector reference](../../docs/configuration.md#mounted-connectors)
defines the `providers` format and default-slot requirements. The
[Bedrock](../../docs/providers/bedrock.md#authentication) and
[Azure](../../docs/providers/azure.md#authentication) guides describe cloud
authentication; prefer workload identity where available.
