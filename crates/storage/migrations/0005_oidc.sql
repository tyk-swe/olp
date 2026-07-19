-- Single-provider OIDC configuration and one-time Authorization Code flow
-- state. Authorization codes and tokens are never persisted. PKCE verifier
-- and nonce material are encrypted with the installation master key.

ALTER TABLE oidc_configurations
    ADD COLUMN singleton boolean NOT NULL DEFAULT true CHECK (singleton),
    ADD COLUMN discovery_url text,
    ADD COLUMN authorization_endpoint text,
    ADD COLUMN token_endpoint text,
    ADD COLUMN jwks_uri text,
    ADD COLUMN token_endpoint_auth_method text NOT NULL DEFAULT 'client_secret_basic'
        CHECK (token_endpoint_auth_method IN ('client_secret_basic', 'client_secret_post')),
    ADD COLUMN scopes text[] NOT NULL DEFAULT ARRAY['openid', 'email', 'profile']::text[],
    ADD COLUMN email_claim text NOT NULL DEFAULT 'email',
    ADD COLUMN groups_claim text NOT NULL DEFAULT 'groups',
    ADD COLUMN default_role user_role,
    ADD COLUMN etag uuid NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN updated_by uuid REFERENCES users(id),
    ADD CONSTRAINT oidc_client_secret_complete CHECK (
        num_nonnulls(encrypted_client_secret, secret_nonce, secret_key_version) IN (0, 3)
        AND (encrypted_client_secret IS NULL OR octet_length(encrypted_client_secret) >= 16)
        AND (secret_nonce IS NULL OR octet_length(secret_nonce) = 12)
        AND (secret_key_version IS NULL OR secret_key_version > 0)
    ),
    ADD CONSTRAINT oidc_scopes_valid CHECK (
        cardinality(scopes) BETWEEN 1 AND 20 AND 'openid' = ANY(scopes)
    ),
    ADD CONSTRAINT oidc_claim_names_valid CHECK (
        email_claim ~ '^[A-Za-z0-9_.:-]{1,128}$'
        AND groups_claim ~ '^[A-Za-z0-9_.:-]{1,128}$'
    ),
    ADD CONSTRAINT oidc_endpoint_lengths CHECK (
        char_length(issuer) BETWEEN 1 AND 2048
        AND char_length(client_id) BETWEEN 1 AND 512
        AND client_id !~ '[[:cntrl:]]'
        AND (discovery_url IS NULL OR char_length(discovery_url) BETWEEN 1 AND 2048)
        AND (authorization_endpoint IS NULL OR char_length(authorization_endpoint) BETWEEN 1 AND 2048)
        AND (token_endpoint IS NULL OR char_length(token_endpoint) BETWEEN 1 AND 2048)
        AND (jwks_uri IS NULL OR char_length(jwks_uri) BETWEEN 1 AND 2048)
    ),
    ADD CONSTRAINT oidc_enabled_configuration_complete CHECK (
        NOT enabled OR (
            discovery_url IS NOT NULL
            AND authorization_endpoint IS NOT NULL
            AND token_endpoint IS NOT NULL
            AND jwks_uri IS NOT NULL
            AND encrypted_client_secret IS NOT NULL
        )
    );

CREATE UNIQUE INDEX oidc_single_configuration_idx
    ON oidc_configurations(singleton);

CREATE TABLE oidc_email_role_mappings (
    configuration_id uuid NOT NULL REFERENCES oidc_configurations(id) ON DELETE CASCADE,
    email text NOT NULL CHECK (
        email = lower(btrim(email)) AND char_length(email) BETWEEN 3 AND 254
        AND email !~ '[[:cntrl:]]'
    ),
    role user_role NOT NULL,
    PRIMARY KEY (configuration_id, email)
);

CREATE TABLE oidc_group_role_mappings (
    configuration_id uuid NOT NULL REFERENCES oidc_configurations(id) ON DELETE CASCADE,
    group_name text NOT NULL CHECK (
        group_name = btrim(group_name) AND char_length(group_name) BETWEEN 1 AND 256
        AND group_name !~ '[[:cntrl:]]'
    ),
    role user_role NOT NULL,
    PRIMARY KEY (configuration_id, group_name)
);

CREATE TABLE oidc_authorization_flows (
    id uuid PRIMARY KEY,
    configuration_id uuid NOT NULL REFERENCES oidc_configurations(id) ON DELETE CASCADE,
    purpose text NOT NULL CHECK (purpose IN ('login', 'link')),
    actor_user_id uuid REFERENCES users(id) ON DELETE CASCADE,
    state_digest bytea NOT NULL UNIQUE CHECK (octet_length(state_digest) = 32),
    browser_binding_digest bytea NOT NULL CHECK (octet_length(browser_binding_digest) = 32),
    encrypted_payload bytea NOT NULL CHECK (octet_length(encrypted_payload) >= 16),
    payload_nonce bytea NOT NULL CHECK (octet_length(payload_nonce) = 12),
    payload_key_version integer NOT NULL CHECK (payload_key_version > 0),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (purpose = 'login' AND actor_user_id IS NULL)
        OR (purpose = 'link' AND actor_user_id IS NOT NULL)
    ),
    CHECK (expires_at > created_at)
);
CREATE INDEX oidc_authorization_flows_expires_at_idx
    ON oidc_authorization_flows(expires_at);

ALTER TABLE oidc_identities
    ADD COLUMN email_at_link text,
    ADD COLUMN last_login_at timestamptz,
    ADD CONSTRAINT oidc_identity_subject_length CHECK (char_length(subject) BETWEEN 1 AND 255),
    ADD CONSTRAINT oidc_identity_issuer_length CHECK (char_length(issuer) BETWEEN 1 AND 2048),
    ADD CONSTRAINT oidc_identity_email_length CHECK (
        email_at_link IS NULL OR (
            char_length(email_at_link) BETWEEN 3 AND 254
            AND email_at_link !~ '[[:cntrl:]]'
        )
    );
