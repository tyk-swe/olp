-- OpenLLMProxy V2 starts with a fresh PostgreSQL database. This schema has no
-- compatibility contract with the legacy SQLite/D1 layouts.

CREATE TYPE user_role AS ENUM ('owner', 'operator', 'developer', 'viewer');
CREATE TYPE provider_state AS ENUM ('draft', 'active', 'disabled');
CREATE TYPE route_draft_state AS ENUM ('draft', 'validated');
CREATE TYPE media_job_state AS ENUM ('queued', 'running', 'succeeded', 'failed', 'cancelled');

CREATE TABLE installation (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    organization_name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id uuid PRIMARY KEY,
    email text NOT NULL,
    display_name text NOT NULL,
    password_hash text,
    role user_role NOT NULL,
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_email_normalized CHECK (email = lower(btrim(email))),
    CONSTRAINT users_email_unique UNIQUE (email)
);

CREATE TABLE invitations (
    id uuid PRIMARY KEY,
    email text NOT NULL,
    role user_role NOT NULL,
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    invited_by uuid NOT NULL REFERENCES users(id),
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    csrf_digest bytea NOT NULL CHECK (octet_length(csrf_digest) = 32),
    expires_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_id_idx ON sessions(user_id);
CREATE INDEX sessions_expires_at_idx ON sessions(expires_at);

CREATE TABLE idempotency_records (
    id uuid PRIMARY KEY,
    actor_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    operation text NOT NULL,
    idempotency_key text NOT NULL,
    state text NOT NULL CHECK (state IN ('in_progress', 'completed')),
    resource_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    UNIQUE (actor_user_id, operation, idempotency_key),
    CHECK (idempotency_key ~ '^[A-Za-z0-9._-]{8,128}$')
);
CREATE INDEX idempotency_records_expires_at_idx ON idempotency_records(expires_at);

CREATE TABLE oidc_configurations (
    id uuid PRIMARY KEY,
    issuer text NOT NULL,
    client_id text NOT NULL,
    encrypted_client_secret bytea,
    secret_nonce bytea,
    secret_key_version integer,
    enabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE oidc_identities (
    issuer text NOT NULL,
    subject text NOT NULL,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject),
    UNIQUE (user_id, issuer)
);

CREATE TABLE providers (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE,
    kind text NOT NULL,
    state provider_state NOT NULL DEFAULT 'draft',
    endpoint text,
    cloud_region text,
    api_version text,
    auth_mode text NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE provider_credential_versions (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    version integer NOT NULL CHECK (version > 0),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) >= 16),
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    master_key_version integer NOT NULL CHECK (master_key_version > 0),
    created_by uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    UNIQUE (provider_id, version)
);

ALTER TABLE providers ADD COLUMN active_credential_version_id uuid
    REFERENCES provider_credential_versions(id);

-- An active credential must belong to the provider selecting it. The deferred
-- composite reference permits the intentional provider/credential insertion
-- cycle while retaining referential integrity for rotation and deletion.
ALTER TABLE provider_credential_versions ADD CONSTRAINT provider_credential_owner_key
    UNIQUE (provider_id, id);
ALTER TABLE providers ADD CONSTRAINT providers_active_credential_owner_fk
    FOREIGN KEY (id, active_credential_version_id)
    REFERENCES provider_credential_versions(provider_id, id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE provider_models (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    upstream_model text NOT NULL,
    display_name text NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    discovered_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_id, upstream_model)
);

CREATE TABLE model_capabilities (
    provider_model_id uuid NOT NULL REFERENCES provider_models(id) ON DELETE CASCADE,
    operation text NOT NULL,
    surface text NOT NULL,
    mode text NOT NULL,
    source text NOT NULL CHECK (source IN ('declared', 'probed', 'certified')),
    certified_at timestamptz,
    PRIMARY KEY (provider_model_id, operation, surface, mode)
);

CREATE TABLE route_drafts (
    id uuid PRIMARY KEY,
    slug text NOT NULL,
    state route_draft_state NOT NULL DEFAULT 'draft',
    overall_timeout_ms integer NOT NULL CHECK (overall_timeout_ms > 0),
    max_attempts smallint NOT NULL CHECK (max_attempts > 0),
    etag uuid NOT NULL,
    based_on_revision_id uuid,
    created_by uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT route_drafts_slug CHECK (
        slug ~ '^[a-z0-9]$'
        OR (slug ~ '^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$' AND position('--' in slug) = 0)
    )
);

CREATE TABLE route_draft_operations (
    route_draft_id uuid NOT NULL REFERENCES route_drafts(id) ON DELETE CASCADE,
    operation text NOT NULL,
    PRIMARY KEY (route_draft_id, operation)
);

CREATE TABLE route_draft_targets (
    id uuid PRIMARY KEY,
    route_draft_id uuid NOT NULL REFERENCES route_drafts(id) ON DELETE CASCADE,
    provider_model_id uuid NOT NULL REFERENCES provider_models(id),
    priority integer NOT NULL CHECK (priority >= 0),
    weight integer NOT NULL CHECK (weight > 0),
    timeout_ms integer NOT NULL CHECK (timeout_ms > 0),
    position integer NOT NULL CHECK (position >= 0),
    UNIQUE (route_draft_id, position)
);

CREATE TABLE routes (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE,
    created_by uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT routes_slug CHECK (
        slug ~ '^[a-z0-9]$'
        OR (slug ~ '^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$' AND position('--' in slug) = 0)
    )
);

CREATE TABLE route_revisions (
    id uuid PRIMARY KEY,
    route_id uuid NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    revision integer NOT NULL CHECK (revision > 0),
    slug text NOT NULL,
    overall_timeout_ms integer NOT NULL CHECK (overall_timeout_ms > 0),
    max_attempts smallint NOT NULL CHECK (max_attempts > 0),
    source_draft_id uuid NOT NULL REFERENCES route_drafts(id),
    activated_by uuid NOT NULL REFERENCES users(id),
    activated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (route_id, revision),
    CONSTRAINT route_revisions_slug CHECK (
        slug ~ '^[a-z0-9]$'
        OR (slug ~ '^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$' AND position('--' in slug) = 0)
    )
);

ALTER TABLE route_drafts ADD CONSTRAINT route_drafts_based_on_revision_fk
    FOREIGN KEY (based_on_revision_id) REFERENCES route_revisions(id);

CREATE TABLE route_revision_operations (
    route_revision_id uuid NOT NULL REFERENCES route_revisions(id) ON DELETE CASCADE,
    operation text NOT NULL,
    PRIMARY KEY (route_revision_id, operation)
);

CREATE TABLE route_revision_targets (
    id uuid PRIMARY KEY,
    route_revision_id uuid NOT NULL REFERENCES route_revisions(id) ON DELETE CASCADE,
    provider_model_id uuid NOT NULL REFERENCES provider_models(id),
    priority integer NOT NULL CHECK (priority >= 0),
    weight integer NOT NULL CHECK (weight > 0),
    timeout_ms integer NOT NULL CHECK (timeout_ms > 0),
    position integer NOT NULL CHECK (position >= 0),
    UNIQUE (route_revision_id, position)
);

CREATE TABLE api_keys (
    id uuid PRIMARY KEY,
    lookup_id text NOT NULL UNIQUE CHECK (lookup_id ~ '^[A-Za-z0-9_]{8,40}$'),
    secret_digest bytea NOT NULL CHECK (octet_length(secret_digest) = 32),
    name text NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id),
    expires_at timestamptz,
    revoked_at timestamptz,
    requests_per_minute integer CHECK (requests_per_minute > 0),
    tokens_per_minute bigint CHECK (tokens_per_minute > 0),
    max_concurrency integer CHECK (max_concurrency > 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_key_scopes (
    api_key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    scope text NOT NULL,
    PRIMARY KEY (api_key_id, scope)
);

CREATE TABLE api_key_route_allowlist (
    api_key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    route_slug text NOT NULL,
    PRIMARY KEY (api_key_id, route_slug)
);

CREATE TABLE runtime_generations (
    id uuid PRIMARY KEY,
    sequence bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    compiled_release bytea NOT NULL,
    release_sha256 bytea NOT NULL CHECK (octet_length(release_sha256) = 32),
    created_by uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE transactional_outbox (
    id uuid PRIMARY KEY,
    topic text NOT NULL,
    aggregate_id uuid NOT NULL,
    payload bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX transactional_outbox_pending_idx
    ON transactional_outbox(created_at) WHERE published_at IS NULL;

CREATE TABLE pricing_revisions (
    id uuid PRIMARY KEY,
    revision integer NOT NULL UNIQUE,
    effective_at timestamptz NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE prices (
    pricing_revision_id uuid NOT NULL REFERENCES pricing_revisions(id) ON DELETE CASCADE,
    provider_kind text NOT NULL,
    model text NOT NULL,
    operation text NOT NULL,
    input_per_million numeric(24, 12),
    output_per_million numeric(24, 12),
    unit_price numeric(24, 12),
    currency char(3) NOT NULL DEFAULT 'USD',
    PRIMARY KEY (pricing_revision_id, provider_kind, model, operation)
);

CREATE TABLE requests (
    id uuid NOT NULL,
    runtime_generation_id uuid NOT NULL REFERENCES runtime_generations(id),
    api_key_id uuid NOT NULL REFERENCES api_keys(id),
    route_slug text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    status_code integer,
    error_class text,
    total_latency_ms integer,
    first_byte_ms integer,
    attempt_count smallint NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, started_at),
    CHECK (status_code IS NULL OR status_code BETWEEN 100 AND 599),
    CHECK (total_latency_ms IS NULL OR total_latency_ms >= 0),
    CHECK (first_byte_ms IS NULL OR first_byte_ms >= 0)
) PARTITION BY RANGE (started_at);

CREATE TABLE requests_default PARTITION OF requests DEFAULT;
CREATE INDEX requests_default_started_at_idx ON requests_default(started_at DESC);
CREATE INDEX requests_default_route_idx ON requests_default(route_slug, started_at DESC);

CREATE TABLE attempts (
    id uuid PRIMARY KEY,
    request_id uuid NOT NULL,
    request_started_at timestamptz NOT NULL,
    ordinal smallint NOT NULL CHECK (ordinal > 0),
    provider_id uuid NOT NULL REFERENCES providers(id),
    upstream_model text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    status_code integer,
    error_class text,
    committed boolean NOT NULL DEFAULT false,
    latency_ms integer,
    first_byte_ms integer,
    UNIQUE (request_id, ordinal),
    CHECK (status_code IS NULL OR status_code BETWEEN 100 AND 599),
    CHECK (latency_ms IS NULL OR latency_ms >= 0),
    CHECK (first_byte_ms IS NULL OR first_byte_ms >= 0),
    FOREIGN KEY (request_id, request_started_at)
        REFERENCES requests(id, started_at) ON DELETE CASCADE
);
CREATE INDEX attempts_request_id_idx ON attempts(request_id);

CREATE TABLE usage_facts (
    id uuid PRIMARY KEY,
    request_id uuid NOT NULL UNIQUE,
    request_started_at timestamptz NOT NULL,
    api_key_id uuid NOT NULL REFERENCES api_keys(id),
    provider_id uuid NOT NULL REFERENCES providers(id),
    route_slug text NOT NULL,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    observed_at timestamptz NOT NULL,
    input_tokens bigint,
    output_tokens bigint,
    cached_input_tokens bigint,
    media_units numeric(24, 6),
    estimated_cost numeric(24, 12),
    unpriced boolean NOT NULL,
    usage_complete boolean NOT NULL,
    pricing_revision_id uuid REFERENCES pricing_revisions(id),
    CHECK (input_tokens IS NULL OR input_tokens >= 0),
    CHECK (output_tokens IS NULL OR output_tokens >= 0),
    CHECK (cached_input_tokens IS NULL OR cached_input_tokens >= 0),
    CHECK (media_units IS NULL OR media_units >= 0),
    CHECK (estimated_cost IS NULL OR estimated_cost >= 0),
    CHECK ((estimated_cost IS NULL AND unpriced) OR (estimated_cost IS NOT NULL AND NOT unpriced)),
    FOREIGN KEY (request_id, request_started_at)
        REFERENCES requests(id, started_at) ON DELETE CASCADE
);
CREATE INDEX usage_facts_observed_at_idx ON usage_facts(observed_at DESC);
CREATE INDEX usage_facts_route_idx ON usage_facts(route_slug, observed_at DESC);

CREATE TABLE usage_hourly (
    bucket timestamptz NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL REFERENCES providers(id),
    upstream_model text NOT NULL,
    operation text NOT NULL,
    request_count bigint NOT NULL,
    input_tokens numeric(30, 0) NOT NULL,
    output_tokens numeric(30, 0) NOT NULL,
    estimated_cost numeric(30, 12),
    unpriced_count bigint NOT NULL,
    incomplete_count bigint NOT NULL,
    CHECK (request_count >= 0),
    CHECK (input_tokens >= 0),
    CHECK (output_tokens >= 0),
    CHECK (estimated_cost IS NULL OR estimated_cost >= 0),
    CHECK (unpriced_count >= 0),
    CHECK (incomplete_count >= 0),
    PRIMARY KEY (bucket, route_slug, provider_id, upstream_model, operation)
);

CREATE TABLE usage_ingestion_gaps (
    id uuid PRIMARY KEY,
    gateway_instance text NOT NULL,
    event_count bigint NOT NULL CHECK (event_count > 0),
    reason text NOT NULL,
    first_observed_at timestamptz NOT NULL,
    last_observed_at timestamptz NOT NULL,
    reported_at timestamptz NOT NULL DEFAULT now(),
    CHECK (last_observed_at >= first_observed_at)
);

CREATE TABLE async_media_jobs (
    id uuid PRIMARY KEY,
    upstream_job_id text,
    api_key_id uuid NOT NULL REFERENCES api_keys(id),
    provider_id uuid NOT NULL REFERENCES providers(id),
    route_slug text NOT NULL,
    operation text NOT NULL,
    state media_job_state NOT NULL,
    expires_at timestamptz,
    error_class text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_events (
    id uuid PRIMARY KEY,
    actor_user_id uuid REFERENCES users(id),
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text,
    outcome text NOT NULL,
    source_ip inet,
    user_agent_family text,
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_occurred_at_idx ON audit_events(occurred_at DESC);

CREATE TABLE settings (
    key text PRIMARY KEY,
    value text NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL REFERENCES users(id),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Enforce the last-owner invariant in the database so every control-plane path,
-- including future ones, receives the same protection.
CREATE OR REPLACE FUNCTION prevent_last_owner_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    removes_active_owner boolean := false;
BEGIN
    IF OLD.role = 'owner' AND OLD.active THEN
        IF TG_OP = 'DELETE' THEN
            removes_active_owner := true;
        ELSIF TG_OP = 'UPDATE' THEN
            removes_active_owner := NEW.role <> 'owner' OR NOT NEW.active;
        END IF;
    END IF;

    IF removes_active_owner THEN
        -- Serialize removal attempts across control replicas. This shares the
        -- installation lock used by first-run setup.
        PERFORM pg_advisory_xact_lock(87189184534066);
    END IF;

    IF removes_active_owner
       AND NOT EXISTS (
           SELECT 1 FROM users
           WHERE id <> OLD.id AND role = 'owner' AND active
       )
    THEN
        RAISE EXCEPTION 'cannot remove or demote the last active owner'
            USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_last_owner_update_guard
BEFORE UPDATE OF role, active ON users
FOR EACH ROW EXECUTE FUNCTION prevent_last_owner_change();

CREATE TRIGGER users_last_owner_delete_guard
BEFORE DELETE ON users
FOR EACH ROW EXECUTE FUNCTION prevent_last_owner_change();
