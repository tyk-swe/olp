CREATE TABLE olp.installation (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    id uuid NOT NULL UNIQUE,
    name text NOT NULL DEFAULT 'OpenLLMProxy',
    setup_complete boolean NOT NULL DEFAULT false,
    auth_fingerprint bytea,
    active_key_version integer,
    authority_id uuid NOT NULL,
    authority_sequence bigint NOT NULL DEFAULT 0
);
CREATE TABLE olp.users (
    id uuid PRIMARY KEY,
    email text NOT NULL UNIQUE CHECK (email = lower(email)),
    display_name text NOT NULL,
    password_hash text,
    role text NOT NULL CHECK (role IN ('owner','operator','developer','viewer')),
    active boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE olp.sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES olp.users,
    digest bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user ON olp.sessions(user_id);
CREATE TABLE olp.recent_auth (
    digest bytea PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES olp.sessions ON DELETE CASCADE,
    purpose text NOT NULL CHECK (purpose IN ('password_enrollment','oidc_link','oidc_unlink')),
    resource_id uuid,
    expires_at timestamptz NOT NULL
);
CREATE TABLE olp.invitations (
    id uuid PRIMARY KEY,
    email text NOT NULL,
    role text NOT NULL CHECK (role IN ('owner','operator','developer','viewer')),
    digest bytea NOT NULL UNIQUE,
    invited_by uuid NOT NULL REFERENCES olp.users,
    accepted_by uuid REFERENCES olp.users,
    revoked_by uuid REFERENCES olp.users,
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE olp.settings (
    key text PRIMARY KEY,
    value text NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL REFERENCES olp.users,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE olp.api_keys (
    id uuid PRIMARY KEY,
    lookup_id text NOT NULL UNIQUE,
    digest bytea NOT NULL,
    name text NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    policy jsonb NOT NULL,
    etag uuid NOT NULL,
    expires_at timestamptz,
    revoked_at timestamptz,
    rotated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE olp.audit (
    id uuid PRIMARY KEY,
    actor_user_id uuid REFERENCES olp.users,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text,
    outcome text NOT NULL CHECK (outcome IN ('success','failure')),
    source_ip text,
    user_agent_family text,
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_time ON olp.audit(occurred_at DESC, id DESC);
-- All encrypted values use the same record-bound envelope and rotation path.
-- Feature tables retain ownership through a matching immutable UUID.
CREATE TABLE olp.secrets (
    id uuid PRIMARY KEY,
    purpose text NOT NULL CHECK (purpose IN ('oidc_client','oidc_flow','mutation_replay')),
    key_version integer NOT NULL,
    ciphertext bytea NOT NULL,
    expires_at timestamptz
);
CREATE TABLE olp.replays (
    actor uuid NOT NULL REFERENCES olp.users,
    key text NOT NULL,
    fingerprint bytea NOT NULL,
    secret_id uuid REFERENCES olp.secrets ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY(actor, key)
);
CREATE TABLE olp.oidc_configuration (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    id uuid NOT NULL UNIQUE,
    document jsonb NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL REFERENCES olp.users
);
CREATE TABLE olp.oidc_identities (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES olp.users,
    issuer text NOT NULL,
    subject text NOT NULL,
    email_at_link text,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    UNIQUE(issuer, subject)
);
CREATE INDEX oidc_identities_user ON olp.oidc_identities(user_id);
CREATE TABLE olp.oidc_flows (
    id uuid PRIMARY KEY REFERENCES olp.secrets ON DELETE CASCADE,
    state_digest bytea NOT NULL UNIQUE,
    cookie_digest bytea NOT NULL,
    configuration_etag uuid NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE TABLE olp.auth_admission (
    action text NOT NULL,
    digest bytea NOT NULL,
    window_started_at timestamptz NOT NULL DEFAULT now(),
    attempts integer NOT NULL,
    PRIMARY KEY(action, digest)
);
