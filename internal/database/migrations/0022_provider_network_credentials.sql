-- Network identity is provider-owned and encrypted in the existing secrets store.
-- Separate references prevent a private TLS key becoming an API-key slot value.
CREATE TABLE olp_go.provider_network_credentials (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES olp_go.providers ON DELETE CASCADE,
    version integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    UNIQUE (provider_id, version)
);
