ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp.secrets ADD CONSTRAINT secrets_purpose_check
    CHECK (purpose IN ('oidc_client','oidc_flow','mutation_replay','provider_credential'));
ALTER TABLE olp.installation ADD COLUMN release_sequence bigint NOT NULL DEFAULT 0;
CREATE TABLE olp.providers (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    kind text NOT NULL,
    state text NOT NULL CHECK (state IN ('draft','active','disabled')),
    configuration jsonb NOT NULL,
    etag uuid NOT NULL,
    slots_etag uuid NOT NULL,
    draft_dirty boolean NOT NULL DEFAULT true,
    active_revision integer,
    active_revision_id uuid,
    last_probe_at timestamptz,
    last_probe_status text,
    last_probe_detail text,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX providers_name ON olp.providers (lower(name));
CREATE TABLE olp.provider_models (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES olp.providers ON DELETE CASCADE,
    upstream_model text NOT NULL,
    display_name text NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    capabilities jsonb NOT NULL DEFAULT '[]',
    discovered_at timestamptz,
    UNIQUE (provider_id, upstream_model)
);
CREATE TABLE olp.provider_credentials (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES olp.providers ON DELETE CASCADE,
    version integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    UNIQUE (provider_id, version)
);
CREATE TABLE olp.provider_slots (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES olp.providers ON DELETE CASCADE,
    is_default boolean NOT NULL DEFAULT false,
    position integer NOT NULL,
    name text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    priority integer NOT NULL DEFAULT 0,
    weight integer NOT NULL DEFAULT 1,
    credential_id uuid REFERENCES olp.provider_credentials,
    restrictions jsonb NOT NULL DEFAULT '{}',
    limits jsonb NOT NULL DEFAULT '{}',
    validated_at timestamptz,
    validated_fingerprint text,
    UNIQUE (provider_id, position)
);
CREATE UNIQUE INDEX provider_slots_default ON olp.provider_slots (provider_id) WHERE is_default;
CREATE TABLE olp.provider_revisions (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES olp.providers ON DELETE CASCADE,
    revision integer NOT NULL,
    name text NOT NULL,
    configuration jsonb NOT NULL,
    models jsonb NOT NULL,
    slots jsonb NOT NULL,
    credential_version integer,
    source_etag uuid NOT NULL,
    activated_by uuid NOT NULL REFERENCES olp.users,
    activated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_id, revision)
);
CREATE TABLE olp.route_drafts (
    id uuid PRIMARY KEY,
    slug text NOT NULL,
    state text NOT NULL CHECK (state IN ('draft','validated')),
    operations jsonb NOT NULL,
    overall_timeout_ms integer NOT NULL,
    max_attempts integer NOT NULL,
    targets jsonb NOT NULL,
    based_on_revision_id uuid,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE olp.routes (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    latest_revision integer NOT NULL,
    latest_revision_id uuid NOT NULL
);
CREATE TABLE olp.route_revisions (
    id uuid PRIMARY KEY,
    route_id uuid NOT NULL REFERENCES olp.routes ON DELETE CASCADE,
    revision integer NOT NULL,
    slug text NOT NULL,
    operations jsonb NOT NULL,
    overall_timeout_ms integer NOT NULL,
    max_attempts integer NOT NULL,
    targets jsonb NOT NULL,
    source_draft_id uuid NOT NULL,
    activated_by uuid NOT NULL REFERENCES olp.users,
    activated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (route_id, revision)
);
CREATE TABLE olp.runtime_releases (
    id uuid PRIMARY KEY,
    sequence bigint NOT NULL UNIQUE,
    sha256 text NOT NULL,
    snapshot jsonb NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz NOT NULL DEFAULT now()
);
