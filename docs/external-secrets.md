# External secrets and wrapped master keys

OLP can unwrap its encryption keys at startup and resolve immutable provider
credential versions from AWS, Google Cloud, Azure or Vault. The deployment's
workload identity supplies access; provider credentials remain in their store.
All service and identity exchanges use the provider egress policy, verified TLS,
bounded deadlines and response sizes, and refuse redirects.

## Wrapped key rings

Mount an owner-readable ring through `OLP_MASTER_KEY_FILE`. Each version contains
exactly one `key` (the existing 32-byte hex/base64 format) or `wrapped` entry:

```json
{
  "active_version": 2,
  "keys": [
    {"version": 1, "wrapped": {"store": "aws", "region": "us-east-1", "key_id": "arn:aws:kms:us-east-1:123456789012:key/key-id", "ciphertext": "BASE64_CIPHERTEXT"}},
    {"version": 2, "wrapped": {"store": "gcp", "key_id": "projects/project/locations/global/keyRings/olp/cryptoKeys/storage", "ciphertext": "BASE64_CIPHERTEXT"}}
  ]
}
```

AWS uses KMS Decrypt; optional `context` must match the encryption context used
to wrap the key. Google Cloud uses Cloud KMS Decrypt with CRC32C verification.
Azure uses a versioned `https://VAULT.vault.azure.net/keys/NAME/VERSION` key ID,
RSA-OAEP-256 and base64url ciphertext. Vault uses a
`https://VAULT/v1/transit/keys/KEY` key ID and its `vault:vN:...` ciphertext.
Every unwrapped result must be exactly 32 bytes. An unavailable or unauthorized
key prevents startup; OLP never substitutes another version.

Use the existing `master-key status`, `master-key reencrypt` and
`master-key verify-retirement` commands with the mounted wrapped ring. Keep old
versions while records or restored backups still need them. Rotate the
installation's active version and re-encrypt records before retiring the old
wrapping key. Changing only the KMS wrapping key leaves OLP's data-key version
and sealed-record bindings unchanged.

## Workload identities

AWS uses the SDK credential chain, including web identity and workload roles.
Google uses Application Default Credentials, including workload federation.
Azure uses `AZURE_TENANT_ID`, `AZURE_CLIENT_ID` and
`AZURE_FEDERATED_TOKEN_FILE` for federation, otherwise managed identity with an
optional client ID. Grant only decrypt access for the selected wrapping keys
and read access to the specified provider-secret versions.

Vault exchanges a mounted JWT using `OLP_VAULT_ROLE`, `OLP_VAULT_JWT_FILE` and
optional `OLP_VAULT_AUTH_MOUNT` (default `jwt`). The JWT file must be a regular
owner-readable file with mode 0400, 0440, 0600 or 0640. OLP rereads it for each
exchange and sends the returned token only to that Vault origin.

Private Vault and metadata-service destinations need explicit
`OLP_PROVIDER_EGRESS_ALLOW_CIDRS` entries. Plain HTTP identity endpoints also
need `OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS`; production secret stores should use
HTTPS. Mount federation files and CA trust into every gateway, control and
worker process that resolves credentials or unwraps the ring. The Helm chart's
extra environment, volume and mount settings support these identity files.

## Pinned provider references

Provider creation, credential rotation and slot writes accept
`credential_reference` instead of a plaintext `credential`:

```json
{"store":"gcp","secret_id":"projects/project/secrets/provider","version":"3"}
```

AWS Secrets Manager requires `region`, a secret ID or ARN and an immutable
32–64 character version ID. Google Secret Manager requires the project/secret
resource and a numeric version. Azure Key Vault uses
`https://VAULT.vault.azure.net/secrets/NAME` and a 32-character version. Vault
KV v2 uses `https://VAULT/v1/MOUNT/data/PATH`, a numeric version and the `field`
containing the credential. Aliases such as `latest` are refused. References
cannot be used for grant enrollment or provider network identities.

The console offers an external-version choice when creating providers, adding
slots or staging rotation. Validate the slot and certify its models before
activation. Rotation resolves and validates the new version against the
upstream before staging it. Published revisions retain their immutable OLP
credential ID and store version until explicitly activated.

Reference-only versions store metadata rather than copied credential values.
The sole runtime credential source caches at most 128 resolved values for one
minute, coalesces concurrent reads and clears expired or evicted buffers.
Failed reads never serve expired values or fall back to a sealed credential.
Route plans mark unavailable references as
`credential_external_secret_unavailable`. Ordinary revocation and the
60-second authority-age boundary continue to take precedence.

## Configuration promotion

Exports retain logical credential references, without store bindings or
resolved values. Supply destination-specific `external_credential_bindings`
alongside the artifact when planning and applying. A name cannot also appear
in `secret_bindings`:

```json
{"provider/primary":{"store":"gcp","secret_id":"projects/destination/secrets/provider","version":"3"}}
```

The CLI accepts this map through `--external-bindings-file` on `config plan`
and `config apply`. Saved plans exclude the binding map; supply it again when
applying. Reference metadata participates in idempotency and desired-action
comparison. Imported providers remain drafts, and must validate their pinned
destination credential before activation.
