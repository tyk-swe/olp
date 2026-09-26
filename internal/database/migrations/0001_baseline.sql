-- OpenLLMProxy schema. The migration runner creates schema olp and its
-- migrations ledger, then applies this file in the same transaction.

-- Installation and access

CREATE TABLE olp.installation (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    id uuid NOT NULL UNIQUE,
    name text NOT NULL DEFAULT 'OpenLLMProxy',
    setup_complete boolean NOT NULL DEFAULT false,
    auth_fingerprint bytea,
    active_key_version integer,
    authority_id uuid NOT NULL,
    authority_sequence bigint NOT NULL DEFAULT 0,
    release_sequence bigint NOT NULL DEFAULT 0
);

-- Credentials do not determine who manages authorization: role_management
-- records whether local administration, OIDC or provisioning owns the role.
CREATE TABLE olp.users (
    id uuid PRIMARY KEY,
    email text NOT NULL UNIQUE CHECK (email = lower(email)),
    display_name text NOT NULL,
    password_hash text,
    role text NOT NULL CHECK (role IN ('owner','operator','developer','viewer')),
    active boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    role_management text NOT NULL DEFAULT 'local'
        CHECK (role_management IN ('local','oidc','provisioned')),
    oidc_authorized boolean NOT NULL DEFAULT true,
    access_scope text NOT NULL DEFAULT 'global' CHECK (access_scope IN ('global','assigned'))
);

-- browser_hint is a coarse display hint, never authentication evidence; no raw
-- user agent or address is retained.
CREATE TABLE olp.sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES olp.users,
    digest bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    browser_hint text NOT NULL
);
CREATE INDEX sessions_user ON olp.sessions(user_id);
CREATE INDEX sessions_expiry ON olp.sessions(expires_at, id);

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
CREATE INDEX invitations_email ON olp.invitations(email) WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TABLE olp.settings (
    key text PRIMARY KEY,
    value text NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL REFERENCES olp.users,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- All encrypted values use the same record-bound envelope and rotation path.
-- Feature tables retain ownership through a matching immutable UUID.
CREATE TABLE olp.secrets (
    id uuid PRIMARY KEY,
    purpose text NOT NULL CHECK (purpose IN ('oidc_client', 'oidc_flow', 'mutation_replay',
                                             'provider_credential', 'notification_secret',
                                             'provider_continuation', 'media_job_source')),
    key_version integer NOT NULL,
    ciphertext bytea NOT NULL,
    expires_at timestamptz,
    CONSTRAINT secrets_continuation_ciphertext_bound
        CHECK (purpose <> 'provider_continuation'
               OR (expires_at IS NOT NULL AND octet_length(ciphertext) <= 5242880)),
    CONSTRAINT secrets_media_job_source_bound
        CHECK (purpose <> 'media_job_source'
               OR (expires_at IS NOT NULL AND octet_length(ciphertext) <= 1048604))
);
CREATE INDEX secrets_expiry ON olp.secrets(expires_at, id) WHERE expires_at IS NOT NULL;

-- A replay actor is a user or a management token, so it has no foreign key.
CREATE TABLE olp.replays (
    actor uuid NOT NULL,
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

-- role_claims are the verified authorization inputs from the latest OIDC
-- sign-in, so configuration changes protect OIDC-only owners with the same
-- role mapping as sign-in. Changing the client secret clears them until the
-- identity signs in again.
CREATE TABLE olp.oidc_identities (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES olp.users,
    issuer text NOT NULL,
    subject text NOT NULL,
    email_at_link text,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    role_claims jsonb,
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
CREATE INDEX auth_admission_expiry ON olp.auth_admission(window_started_at);

CREATE TABLE olp.provisioned_users (
    source text NOT NULL,
    external_id text NOT NULL,
    user_id uuid NOT NULL UNIQUE REFERENCES olp.users,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(source, external_id),
    CHECK (source <> '' AND octet_length(source) <= 100 AND external_id <> '' AND octet_length(external_id) <= 255)
);

-- Projects, budget groups, API keys and management tokens

CREATE TABLE olp.projects (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX projects_name ON olp.projects (lower(name));

CREATE TABLE olp.project_members (
    project_id uuid NOT NULL REFERENCES olp.projects ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES olp.users ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('manager','viewer')),
    added_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(project_id,user_id)
);

CREATE TABLE olp.budget_groups (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    project_id uuid REFERENCES olp.projects,
    daily_cost_limit numeric(24,12),
    monthly_cost_limit numeric(24,12),
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (daily_cost_limit IS NOT NULL OR monthly_cost_limit IS NOT NULL),
    CHECK (daily_cost_limit IS NULL OR daily_cost_limit > 0),
    CHECK (monthly_cost_limit IS NULL OR monthly_cost_limit > 0)
);
CREATE UNIQUE INDEX budget_groups_name_scope ON olp.budget_groups
    (lower(name), COALESCE(project_id,'00000000-0000-0000-0000-000000000000'::uuid));

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
    created_at timestamptz NOT NULL DEFAULT now(),
    project_id uuid REFERENCES olp.projects,
    budget_group_id uuid REFERENCES olp.budget_groups
);

CREATE TABLE olp.management_tokens (
    id uuid PRIMARY KEY,
    lookup_id text NOT NULL UNIQUE,
    digest bytea NOT NULL,
    name text NOT NULL,
    scopes jsonb NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    etag uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    all_projects boolean NOT NULL,
    project_ids jsonb NOT NULL
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
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor_management_token_id uuid REFERENCES olp.management_tokens,
    CONSTRAINT audit_one_actor_check CHECK (
        NOT (actor_user_id IS NOT NULL AND actor_management_token_id IS NOT NULL))
);
CREATE INDEX audit_time ON olp.audit(occurred_at DESC, id DESC);

-- Providers, routes and runtime releases

CREATE TABLE olp.providers (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    kind text NOT NULL,
    state text NOT NULL CHECK (state IN ('draft','active','disabled')),
    configuration json NOT NULL,
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
    updated_at timestamptz NOT NULL DEFAULT now(),
    project_id uuid REFERENCES olp.projects
);
CREATE UNIQUE INDEX providers_name ON olp.providers (lower(name));
COMMENT ON COLUMN olp.providers.configuration IS
    'Authoritative provider configuration; native JSON number spellings and subtrees are preserved.';

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

-- Network identity is provider-owned and encrypted in the secrets store.
-- Separate references prevent a private TLS key becoming an API-key slot value.
CREATE TABLE olp.provider_network_credentials (
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
    configuration json NOT NULL,
    models jsonb NOT NULL,
    slots jsonb NOT NULL,
    credential_version integer,
    source_etag uuid NOT NULL,
    activated_by uuid NOT NULL REFERENCES olp.users,
    activated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_id, revision)
);
COMMENT ON COLUMN olp.provider_revisions.configuration IS
    'Immutable authoritative provider configuration at activation; never normalize through jsonb.';

-- Route fidelity is strict or transformed, and every draft and revision
-- states it.
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
    updated_at timestamptz NOT NULL DEFAULT now(),
    project_id uuid REFERENCES olp.projects,
    content_policy jsonb,
    fidelity jsonb NOT NULL CHECK (
        fidelity IN ('{"mode": "strict"}'::jsonb, '{"mode": "transformed"}'::jsonb)
    )
);

CREATE TABLE olp.routes (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    latest_revision integer NOT NULL,
    latest_revision_id uuid NOT NULL,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','retired')),
    etag uuid NOT NULL,
    retired_at timestamptz,
    retired_by uuid REFERENCES olp.users,
    project_id uuid REFERENCES olp.projects,
    CONSTRAINT routes_retirement_check CHECK (
        (state='active' AND retired_at IS NULL AND retired_by IS NULL)
     OR (state='retired' AND retired_at IS NOT NULL AND retired_by IS NOT NULL))
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
    routing_policy jsonb NOT NULL,
    content_policy jsonb,
    fidelity jsonb NOT NULL CHECK (
        fidelity IN ('{"mode": "strict"}'::jsonb, '{"mode": "transformed"}'::jsonb)
    ),
    UNIQUE (route_id, revision)
);

CREATE FUNCTION olp.preserve_route_slug() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.slug IS DISTINCT FROM OLD.slug THEN
        RAISE EXCEPTION 'route slug is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'route_slug_identity';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER preserve_route_slug BEFORE UPDATE ON olp.routes
FOR EACH ROW EXECUTE FUNCTION olp.preserve_route_slug();

-- A published revision keeps its route, slug and fidelity, and its slug is
-- the slug of its route.
CREATE FUNCTION olp.check_route_revision_identity() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP = 'UPDATE' AND (
        NEW.route_id IS DISTINCT FROM OLD.route_id OR
        NEW.slug IS DISTINCT FROM OLD.slug OR
        NEW.fidelity IS DISTINCT FROM OLD.fidelity
    ) THEN
        RAISE EXCEPTION 'published route revision identity is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'route_revision_identity';
    END IF;
    IF NEW.slug IS DISTINCT FROM (SELECT slug FROM olp.routes WHERE id = NEW.route_id) THEN
        RAISE EXCEPTION 'route revision slug must match its route'
            USING ERRCODE = '23514', CONSTRAINT = 'route_revision_identity';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER check_route_revision_identity BEFORE INSERT OR UPDATE ON olp.route_revisions
FOR EACH ROW EXECUTE FUNCTION olp.check_route_revision_identity();

CREATE TABLE olp.runtime_releases (
    id uuid PRIMARY KEY,
    sequence bigint NOT NULL UNIQUE,
    sha256 text NOT NULL,
    snapshot json NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON COLUMN olp.runtime_releases.snapshot IS
    'Authoritative serving snapshot. Preserve native JSON source and its recorded sha256; never rewrite a digest after normalization.';

CREATE TABLE olp.routing_policies (
    scope text NOT NULL CHECK (scope IN ('installation','route-draft','api-key')),
    scope_id uuid NOT NULL,
    policy jsonb NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL REFERENCES olp.users,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, scope_id)
);

-- Durable accounting: request history, attempt usage facts, pricing revisions,
-- spend windows, and the health rows the recovery workers checkpoint into.
-- Money never leaves the database as a float: accrued spend, rates, and rolled
-- up aggregates are exact `numeric`, and enum-shaped columns are text with a
-- CHECK so migrations stay ordinary DDL.
-- Spend history outlives the key that produced it: the api_key_id references of
-- requests, attempt_usage_facts and attempt_usage_hourly carry no ON DELETE
-- clause, so removing a key is refused rather than silently erasing what it was
-- charged for. Only api_key_cost_windows cascades, because it is a cache the
-- reconciliation pass rebuilds from those facts.

-- Reconstructed spend per budget window. The windows are derived (day number
-- and year*12+month), so a reconciliation pass can rebuild Valkey counters
-- after an outage without replaying facts.
CREATE TABLE olp.api_key_cost_windows (
    api_key_id uuid NOT NULL REFERENCES olp.api_keys ON DELETE CASCADE,
    window_kind text NOT NULL CHECK (window_kind IN ('day','month')),
    window_id bigint NOT NULL CHECK (window_id >= 0),
    accrued numeric(28,12) NOT NULL CHECK (accrued >= 0),
    unpriced_attempts bigint NOT NULL CHECK (unpriced_attempts >= 0),
    CONSTRAINT api_key_cost_windows_unpriced_scope_check
        CHECK (window_kind = 'month' OR unpriced_attempts = 0),
    PRIMARY KEY (api_key_id, window_kind, window_id)
);

CREATE TABLE olp.budget_group_cost_windows (
    budget_group_id uuid NOT NULL REFERENCES olp.budget_groups,
    window_kind text NOT NULL CHECK (window_kind IN ('day','month')),
    window_id bigint NOT NULL CHECK (window_id >= 0),
    accrued numeric(28,12) NOT NULL CHECK (accrued >= 0),
    unpriced_attempts bigint NOT NULL CHECK (unpriced_attempts >= 0),
    CHECK (window_kind='month' OR unpriced_attempts=0),
    PRIMARY KEY(budget_group_id,window_kind,window_id)
);

-- Request history is range partitioned so retention purges whole ranges as the
-- installation grows. A DEFAULT partition keeps the schema complete without a
-- partition maintenance worker.
CREATE TABLE olp.requests (
    id uuid NOT NULL,
    runtime_generation_id uuid NOT NULL,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    route_slug text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    status_code integer CHECK (status_code IS NULL OR (status_code >= 100 AND status_code <= 599)),
    error_class text,
    total_latency_ms integer CHECK (total_latency_ms IS NULL OR total_latency_ms >= 0),
    first_byte_ms integer CHECK (first_byte_ms IS NULL OR first_byte_ms >= 0),
    attempt_count smallint NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    budget_group_id uuid REFERENCES olp.budget_groups,
    attribution jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(attribution) = 'object'
               AND octet_length(attribution::text) <= 1024),
    policy_decisions jsonb NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(policy_decisions) = 'array'
               AND octet_length(policy_decisions::text) <= 16384),
    PRIMARY KEY (id, started_at)
) PARTITION BY RANGE (started_at);
CREATE TABLE olp.requests_default PARTITION OF olp.requests DEFAULT;
CREATE INDEX requests_route_idx ON olp.requests (route_slug, started_at DESC);
CREATE INDEX requests_started_at_idx ON olp.requests (started_at DESC);

CREATE TABLE olp.attempts (
    id uuid PRIMARY KEY,
    request_id uuid NOT NULL,
    request_started_at timestamptz NOT NULL,
    ordinal smallint NOT NULL CHECK (ordinal > 0),
    provider_id uuid NOT NULL REFERENCES olp.providers,
    upstream_model text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    status_code integer CHECK (status_code IS NULL OR (status_code >= 100 AND status_code <= 599)),
    error_class text,
    committed boolean NOT NULL DEFAULT false,
    latency_ms integer CHECK (latency_ms IS NULL OR latency_ms >= 0),
    first_byte_ms integer CHECK (first_byte_ms IS NULL OR first_byte_ms >= 0),
    -- Routing provenance (policy, mode, credential slot, pricing pin) for the
    -- request detail view. Content-free by construction.
    routing jsonb,
    UNIQUE (request_id, ordinal),
    FOREIGN KEY (request_id, request_started_at)
        REFERENCES olp.requests (id, started_at) ON DELETE CASCADE
);
CREATE INDEX attempts_provider_started_idx ON olp.attempts (provider_id, started_at DESC);
CREATE INDEX attempts_request_id_idx ON olp.attempts (request_id);
CREATE INDEX attempts_routing_measurements ON olp.attempts (completed_at DESC)
    WHERE error_class IS NULL AND status_code BETWEEN 200 AND 299;

-- Usage facts outlive their request rows: retention purges history earlier than
-- aggregates. The anchor carries the partition key so facts keep a foreign key
-- without pinning the partitioned history table.
CREATE TABLE olp.usage_request_anchors (
    request_id uuid NOT NULL,
    request_started_at timestamptz NOT NULL,
    PRIMARY KEY (request_id, request_started_at)
);
CREATE INDEX usage_request_anchors_started_at_idx ON olp.usage_request_anchors (request_started_at);

CREATE TABLE olp.pricing_sources (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE,
    url text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE olp.pricing_source_snapshots (
    id uuid PRIMARY KEY,
    source_id uuid NOT NULL REFERENCES olp.pricing_sources,
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    document jsonb NOT NULL CHECK (octet_length(document::text) <= 4194304),
    fetched_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_id, sha256)
);

CREATE TABLE olp.pricing_revisions (
    id uuid PRIMARY KEY,
    revision integer NOT NULL UNIQUE,
    effective_at timestamptz NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    source_snapshot_id uuid REFERENCES olp.pricing_source_snapshots
);

-- Operation labels are admitted by the trusted application registry. SQL
-- retains bounded opaque labels so adding a codec does not require a schema
-- change.
CREATE TABLE olp.prices (
    pricing_revision_id uuid NOT NULL REFERENCES olp.pricing_revisions ON DELETE CASCADE,
    provider_kind text NOT NULL CHECK (provider_kind IN (
        'openai','anthropic','gemini','vertex_ai','bedrock','azure_openai','openai_compatible')),
    model text NOT NULL,
    operation text NOT NULL CHECK (operation ~ '^[a-z][a-z0-9_-]{0,95}$'),
    input_per_million numeric(24,12),
    output_per_million numeric(24,12),
    cached_input_per_million numeric(24,12),
    unit_price numeric(24,12),
    currency char(3) NOT NULL DEFAULT 'USD'
        CHECK (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$'),
    provider_id uuid REFERENCES olp.providers ON DELETE CASCADE,
    -- Vendor catalogue identity (an `openai_compatible` connector fronting a
    -- known vendor prices against that vendor, not the connector kind).
    vendor_id text,
    cache_write_input_per_million numeric(24,12),
    cache_write_5m_input_per_million numeric(24,12),
    cache_write_1h_input_per_million numeric(24,12),
    CONSTRAINT prices_revision_scope_key UNIQUE NULLS NOT DISTINCT
        (pricing_revision_id, provider_kind, provider_id, vendor_id, model, operation)
);
CREATE INDEX prices_provider_override_idx ON olp.prices (provider_id, model, operation)
    WHERE provider_id IS NOT NULL;

-- One installation reports one currency; mixing them would make every total a
-- lie. The first revision sets it and later revisions are checked against it.
CREATE TABLE olp.pricing_currency (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    currency char(3) NOT NULL CHECK (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$')
);

-- One row per provider attempt that produced billable evidence. The CHECK
-- constraints encode the charge state machine so no code path can persist a
-- priced row without complete usage, or a cost on a not-billable attempt.
CREATE TABLE olp.attempt_usage_facts (
    attempt_id uuid PRIMARY KEY,
    event_id uuid NOT NULL,
    request_id uuid NOT NULL,
    request_started_at timestamptz NOT NULL,
    attempt_ordinal smallint NOT NULL CHECK (attempt_ordinal > 0),
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    provider_id uuid NOT NULL REFERENCES olp.providers,
    route_slug text NOT NULL,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL CHECK (surface ~ '^[a-z][a-z0-9_-]{0,95}$'),
    observed_at timestamptz NOT NULL,
    charge_status text NOT NULL CHECK (charge_status IN ('not_billable','billable','billing_uncertain')),
    usage_observed boolean NOT NULL,
    usage_complete boolean NOT NULL,
    input_tokens bigint CHECK (input_tokens IS NULL OR input_tokens >= 0),
    output_tokens bigint CHECK (output_tokens IS NULL OR output_tokens >= 0),
    cached_input_tokens bigint CHECK (cached_input_tokens IS NULL OR cached_input_tokens >= 0),
    media_units numeric(24,6) CHECK (media_units IS NULL OR media_units >= 0),
    estimated_cost numeric(24,12) CHECK (estimated_cost IS NULL OR estimated_cost >= 0),
    unpriced boolean NOT NULL,
    pricing_revision_id uuid REFERENCES olp.pricing_revisions,
    currency char(3) CHECK (currency IS NULL
        OR (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$')),
    request_counted boolean NOT NULL,
    provider_request_counted boolean NOT NULL,
    model_request_counted boolean NOT NULL,
    target_request_counted boolean NOT NULL,
    request_unpriced_counted boolean NOT NULL,
    provider_unpriced_counted boolean NOT NULL,
    model_unpriced_counted boolean NOT NULL,
    target_unpriced_counted boolean NOT NULL,
    request_incomplete_counted boolean NOT NULL,
    provider_incomplete_counted boolean NOT NULL,
    model_incomplete_counted boolean NOT NULL,
    target_incomplete_counted boolean NOT NULL,
    budget_group_id uuid REFERENCES olp.budget_groups,
    cache_write_input_tokens bigint
        CHECK (cache_write_input_tokens IS NULL OR cache_write_input_tokens>=0),
    cache_write_5m_input_tokens bigint
        CHECK (cache_write_5m_input_tokens IS NULL OR cache_write_5m_input_tokens>=0),
    cache_write_1h_input_tokens bigint
        CHECK (cache_write_1h_input_tokens IS NULL OR cache_write_1h_input_tokens>=0),
    attribution jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(attribution) = 'object'
               AND octet_length(attribution::text) <= 1024),
    UNIQUE (request_id, attempt_ordinal),
    FOREIGN KEY (request_id, request_started_at)
        REFERENCES olp.usage_request_anchors (request_id, request_started_at) ON DELETE CASCADE,
    CONSTRAINT attempt_usage_facts_not_billable_check CHECK (
        charge_status <> 'not_billable'
        OR (NOT usage_observed AND usage_complete AND NOT unpriced
            AND estimated_cost IS NULL AND pricing_revision_id IS NULL)),
    CONSTRAINT attempt_usage_facts_billing_uncertain_check CHECK (
        charge_status <> 'billing_uncertain' OR NOT usage_complete),
    CONSTRAINT attempt_usage_facts_priced_check CHECK (
        estimated_cost IS NULL
        OR (charge_status = 'billable' AND usage_complete AND NOT unpriced)),
    CONSTRAINT attempt_usage_cache_subset CHECK (
        (cached_input_tokens IS NULL AND cache_write_input_tokens IS NULL) OR
        (input_tokens IS NOT NULL AND
         COALESCE(cached_input_tokens,0)+COALESCE(cache_write_input_tokens,0)<=input_tokens)),
    CONSTRAINT attempt_usage_cache_detail_subset CHECK (
        (cache_write_5m_input_tokens IS NULL AND cache_write_1h_input_tokens IS NULL) OR
        (cache_write_input_tokens IS NOT NULL AND
         COALESCE(cache_write_5m_input_tokens,0)+COALESCE(cache_write_1h_input_tokens,0)<=cache_write_input_tokens))
);
CREATE INDEX attempt_usage_facts_api_key_observed_at_idx
    ON olp.attempt_usage_facts (api_key_id, observed_at DESC);
CREATE INDEX attempt_usage_facts_event_id_idx ON olp.attempt_usage_facts (event_id);
CREATE INDEX attempt_usage_facts_model_idx
    ON olp.attempt_usage_facts (upstream_model, observed_at DESC);
CREATE INDEX attempt_usage_facts_observed_at_idx ON olp.attempt_usage_facts (observed_at DESC);
CREATE INDEX attempt_usage_facts_provider_idx
    ON olp.attempt_usage_facts (provider_id, observed_at DESC);
CREATE INDEX attempt_usage_facts_request_idx
    ON olp.attempt_usage_facts (request_id, request_started_at, attempt_ordinal);

-- Rolled up usage. Reports read facts and hourly rows as one set, so every
-- count scope of the fact table survives the rollup as a separate column.
CREATE TABLE olp.attempt_usage_hourly (
    bucket timestamptz NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL REFERENCES olp.providers,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL CHECK (surface ~ '^[a-z][a-z0-9_-]{0,95}$'),
    api_key_id uuid REFERENCES olp.api_keys,
    request_count bigint NOT NULL CHECK (request_count >= 0),
    provider_request_count bigint NOT NULL CHECK (provider_request_count >= 0),
    model_request_count bigint NOT NULL CHECK (model_request_count >= 0),
    target_request_count bigint NOT NULL CHECK (target_request_count >= 0),
    input_tokens numeric(30,0) NOT NULL CHECK (input_tokens >= 0),
    output_tokens numeric(30,0) NOT NULL CHECK (output_tokens >= 0),
    cached_input_tokens numeric(30,0) NOT NULL CHECK (cached_input_tokens >= 0),
    media_units numeric(30,6) NOT NULL CHECK (media_units >= 0),
    estimated_cost numeric(30,12) CHECK (estimated_cost >= 0),
    request_unpriced_count bigint NOT NULL CHECK (request_unpriced_count >= 0),
    provider_unpriced_count bigint NOT NULL CHECK (provider_unpriced_count >= 0),
    model_unpriced_count bigint NOT NULL CHECK (model_unpriced_count >= 0),
    target_unpriced_count bigint NOT NULL CHECK (target_unpriced_count >= 0),
    request_incomplete_count bigint NOT NULL CHECK (request_incomplete_count >= 0),
    provider_incomplete_count bigint NOT NULL CHECK (provider_incomplete_count >= 0),
    model_incomplete_count bigint NOT NULL CHECK (model_incomplete_count >= 0),
    target_incomplete_count bigint NOT NULL CHECK (target_incomplete_count >= 0),
    currency char(3) CHECK (currency IS NULL
        OR (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$')),
    unpriced_attempt_count bigint NOT NULL DEFAULT 0 CHECK (unpriced_attempt_count >= 0),
    budget_group_id uuid REFERENCES olp.budget_groups,
    cache_write_input_tokens numeric(30,0) NOT NULL DEFAULT 0 CHECK (cache_write_input_tokens>=0),
    cache_write_5m_input_tokens numeric(30,0) NOT NULL DEFAULT 0 CHECK (cache_write_5m_input_tokens>=0),
    cache_write_1h_input_tokens numeric(30,0) NOT NULL DEFAULT 0 CHECK (cache_write_1h_input_tokens>=0),
    attribution jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(attribution) = 'object'
               AND octet_length(attribution::text) <= 1024),
    -- Named so the rollup can target it by name; api_key_id and budget_group_id
    -- are nullable, and a NULL must still collide with the row it rolled into.
    CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
        (bucket, route_slug, provider_id, upstream_model, operation, surface,
         api_key_id, budget_group_id, attribution)
);
CREATE INDEX attempt_usage_hourly_api_key_bucket_idx
    ON olp.attempt_usage_hourly (api_key_id, bucket DESC) WHERE api_key_id IS NOT NULL;

-- Gaps are the honest record of metadata that was lost or could not be
-- attributed. Reports treat any overlapping gap as evidence of incompleteness.
CREATE TABLE olp.request_metadata_ingestion_gaps (
    id uuid PRIMARY KEY,
    gateway_instance text NOT NULL,
    event_count bigint NOT NULL CHECK (event_count >= 0),
    reason text NOT NULL,
    first_observed_at timestamptz NOT NULL,
    last_observed_at timestamptz NOT NULL,
    reported_at timestamptz NOT NULL DEFAULT now(),
    certainty text NOT NULL DEFAULT 'exact' CHECK (certainty IN ('exact','lower_bound')),
    deduplication_key text CHECK (deduplication_key IS NULL
        OR (deduplication_key <> '' AND octet_length(deduplication_key) <= 256)),
    CONSTRAINT request_metadata_ingestion_gaps_window_check
        CHECK (last_observed_at >= first_observed_at),
    CONSTRAINT request_metadata_ingestion_gaps_count_check
        CHECK (certainty = 'lower_bound' OR event_count > 0)
);
CREATE UNIQUE INDEX request_metadata_ingestion_gaps_deduplication_key_idx
    ON olp.request_metadata_ingestion_gaps (deduplication_key) WHERE deduplication_key IS NOT NULL;

CREATE TABLE olp.request_metadata_gap_hourly (
    bucket timestamptz NOT NULL,
    gateway_instance text NOT NULL,
    reason text NOT NULL,
    event_count bigint NOT NULL CHECK (event_count >= 0),
    first_observed_at timestamptz NOT NULL,
    last_observed_at timestamptz NOT NULL,
    uncertain_gap_count bigint NOT NULL DEFAULT 0 CHECK (uncertain_gap_count >= 0),
    CONSTRAINT request_metadata_gap_hourly_bucket_utc_check
        CHECK (bucket = date_trunc('hour', first_observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),
    CONSTRAINT request_metadata_gap_hourly_window_check
        CHECK (last_observed_at >= first_observed_at),
    CONSTRAINT request_metadata_gap_hourly_evidence_check
        CHECK (event_count > 0 OR uncertain_gap_count > 0),
    PRIMARY KEY (bucket, gateway_instance, reason)
);
CREATE INDEX request_metadata_gap_hourly_overlap_idx
    ON olp.request_metadata_gap_hourly (last_observed_at, first_observed_at);

-- One row per gateway process lifetime. An open epoch that stops checkpointing
-- is detected, marked unclean, and carries the gap that bounds its loss.
CREATE TABLE olp.request_metadata_gateway_epochs (
    gateway_instance text NOT NULL,
    process_epoch uuid NOT NULL,
    started_at timestamptz NOT NULL,
    accepted bigint NOT NULL CHECK (accepted >= 0),
    persisted bigint NOT NULL CHECK (persisted >= 0),
    dropped bigint NOT NULL CHECK (dropped >= 0),
    abandoned bigint NOT NULL CHECK (abandoned >= 0),
    retrying boolean NOT NULL,
    writer_closed boolean NOT NULL,
    updated_at timestamptz NOT NULL,
    gracefully_closed_at timestamptz,
    stale_candidate_at timestamptz,
    stale_detected_at timestamptz,
    acknowledged_at timestamptz,
    acknowledged_by uuid REFERENCES olp.users ON DELETE SET NULL,
    uncertainty_gap_id uuid REFERENCES olp.request_metadata_ingestion_gaps ON DELETE SET NULL,
    CONSTRAINT request_metadata_gateway_epochs_updated_check CHECK (updated_at >= started_at),
    CONSTRAINT request_metadata_gateway_epochs_closed_check
        CHECK (gracefully_closed_at IS NULL OR gracefully_closed_at >= started_at),
    CONSTRAINT request_metadata_gateway_epochs_candidate_check
        CHECK (stale_candidate_at IS NULL OR stale_candidate_at >= started_at),
    CONSTRAINT request_metadata_gateway_epochs_detected_check
        CHECK (stale_detected_at IS NULL OR stale_detected_at >= started_at),
    CONSTRAINT request_metadata_gateway_epochs_acknowledged_check
        CHECK (acknowledged_at IS NULL
            OR (stale_detected_at IS NOT NULL AND acknowledged_at >= stale_detected_at)),
    CONSTRAINT request_metadata_gateway_epochs_resolution_check
        CHECK (NOT (gracefully_closed_at IS NOT NULL AND stale_detected_at IS NOT NULL)),
    PRIMARY KEY (gateway_instance, process_epoch)
);
CREATE UNIQUE INDEX request_metadata_gateway_epochs_process_epoch_idx
    ON olp.request_metadata_gateway_epochs (process_epoch);
CREATE UNIQUE INDEX request_metadata_gateway_epochs_one_open_idx
    ON olp.request_metadata_gateway_epochs (gateway_instance)
    WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL;
CREATE INDEX request_metadata_gateway_epochs_stale_scan_idx
    ON olp.request_metadata_gateway_epochs (updated_at)
    WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL;
CREATE INDEX request_metadata_gateway_epochs_unresolved_idx
    ON olp.request_metadata_gateway_epochs (stale_detected_at)
    WHERE stale_detected_at IS NOT NULL AND acknowledged_at IS NULL;

-- Bounded idempotency for stream delivery. A receipt admits one event once
-- within the supported replay window; older deliveries are rejected explicitly
-- rather than silently added to an aggregate they can no longer belong to. An
-- event describes exactly one request, and a request has exactly one terminal
-- event, so each identity is unique on its own.
CREATE TABLE olp.request_metadata_event_receipts (
    event_id uuid NOT NULL,
    request_id uuid NOT NULL,
    event_sha256 bytea NOT NULL CHECK (octet_length(event_sha256) = 32),
    status text NOT NULL CHECK (status IN ('pending','fact_persisted','rejected')),
    observed_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, request_id)
);
CREATE INDEX request_metadata_event_receipts_recorded_at_idx
    ON olp.request_metadata_event_receipts USING brin (recorded_at);
CREATE UNIQUE INDEX request_metadata_event_receipts_event_identity_idx
    ON olp.request_metadata_event_receipts (event_id);
CREATE UNIQUE INDEX request_metadata_event_receipts_request_identity_idx
    ON olp.request_metadata_event_receipts (request_id);

CREATE TABLE olp.request_metadata_consumer_health (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    pending_events bigint NOT NULL CHECK (pending_events >= 0),
    lag_events bigint NOT NULL CHECK (lag_events >= 0),
    oldest_pending_at timestamptz,
    checked_at timestamptz NOT NULL
);

-- Fixed worker responsibilities checkpoint here; readiness reads staleness from
-- `checked_at` and progress from `last_progress_at`.
CREATE TABLE olp.worker_task_health (
    task text PRIMARY KEY CHECK (task IN ('request_metadata_consumer', 'maintenance',
                                          'cost_reconciliation',
                                          'request_metadata_gateway_epoch_detection',
                                          'media_reconciliation', 'budget_alert_delivery')),
    checked_at timestamptz NOT NULL,
    last_success_at timestamptz,
    last_progress_at timestamptz,
    successes_total bigint NOT NULL DEFAULT 0 CHECK (successes_total >= 0),
    failures_total bigint NOT NULL DEFAULT 0 CHECK (failures_total >= 0),
    skipped_total bigint NOT NULL DEFAULT 0 CHECK (skipped_total >= 0)
);

CREATE TABLE olp.async_worker_counters (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    request_metadata_reclaimed_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_reclaimed_total >= 0),
    request_metadata_recovered_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_recovered_total >= 0),
    request_metadata_duplicates_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_duplicates_total >= 0),
    request_metadata_processed_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_processed_total >= 0),
    media_reconciliation_gaps_total bigint NOT NULL DEFAULT 0
        CHECK (media_reconciliation_gaps_total >= 0)
);
INSERT INTO olp.async_worker_counters (singleton) VALUES (true);

-- Budget alerts

CREATE TABLE olp.notification_destinations (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    url text NOT NULL,
    project_id uuid REFERENCES olp.projects,
    secret_id uuid REFERENCES olp.secrets ON DELETE SET NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX notification_destinations_name_scope
    ON olp.notification_destinations
    (lower(name), COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid));

CREATE TABLE olp.budget_alert_rules (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    project_id uuid REFERENCES olp.projects,
    subject_kind text NOT NULL CHECK (subject_kind IN ('api_key', 'budget_group')),
    subject_id uuid NOT NULL,
    window_kind text NOT NULL CHECK (window_kind IN ('day', 'month')),
    threshold_percent integer NOT NULL CHECK (threshold_percent BETWEEN 1 AND 100),
    destination_id uuid NOT NULL REFERENCES olp.notification_destinations,
    enabled boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (subject_kind, subject_id, window_kind, threshold_percent, destination_id)
);

CREATE TABLE olp.budget_alert_deliveries (
    id uuid PRIMARY KEY,
    rule_id uuid NOT NULL REFERENCES olp.budget_alert_rules,
    window_id bigint NOT NULL,
    threshold_percent integer NOT NULL,
    accrued numeric(28,12) NOT NULL CHECK (accrued >= 0),
    limit_amount numeric(24,12) NOT NULL CHECK (limit_amount > 0),
    currency char(3),
    status text NOT NULL CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5),
    last_error_code text,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_attempt_at timestamptz,
    delivered_at timestamptz,
    UNIQUE (rule_id, window_id, threshold_percent)
);

-- Durable media jobs. Job state is client-visible; lifecycle_state tracks
-- provider identity ownership independently of what the client sees.

CREATE FUNCTION olp.enforce_media_job_lifecycle_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.lifecycle_state = 'deleted' AND NEW.lifecycle_state <> 'deleted' THEN
        RAISE EXCEPTION 'deleted media job lifecycle is terminal'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'creating'
       AND NEW.lifecycle_state NOT IN (
           'creating', 'active', 'create_ambiguous', 'create_cleanup_pending', 'deleted'
       )
    THEN
        RAISE EXCEPTION 'invalid creating media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'active'
       AND NEW.lifecycle_state NOT IN ('active', 'delete_pending')
    THEN
        RAISE EXCEPTION 'invalid active media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'create_ambiguous'
       AND NEW.lifecycle_state NOT IN (
           'create_ambiguous', 'create_cleanup_pending', 'deleted'
       )
    THEN
        RAISE EXCEPTION 'invalid ambiguous media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'create_cleanup_pending'
       AND NEW.lifecycle_state NOT IN ('create_cleanup_pending', 'deleted')
    THEN
        RAISE EXCEPTION 'invalid create cleanup media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'delete_pending'
       AND NEW.lifecycle_state NOT IN ('delete_pending', 'deleted')
    THEN
        RAISE EXCEPTION 'invalid delete-pending media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.lifecycle_state = 'deleted' THEN
        NEW.deleted_at := COALESCE(NEW.deleted_at, now());
        NEW.content_available := false;
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION olp.enforce_media_job_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.state IN ('succeeded', 'failed', 'cancelled') AND NEW.state <> OLD.state THEN
        RAISE EXCEPTION 'terminal media job state cannot transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'queued' AND NEW.state NOT IN ('queued', 'running', 'succeeded', 'failed', 'cancelled') THEN
        RAISE EXCEPTION 'invalid queued media job transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'running' AND NEW.state NOT IN ('running', 'succeeded', 'failed', 'cancelled') THEN
        RAISE EXCEPTION 'invalid running media job transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.progress_percent IS NOT NULL
       AND OLD.progress_percent IS NOT NULL
       AND NEW.progress_percent < OLD.progress_percent
    THEN
        RAISE EXCEPTION 'media job progress cannot decrease'
            USING ERRCODE = 'check_violation';
    END IF;
    NEW.updated_at := now();
    IF NEW.state IN ('succeeded', 'failed', 'cancelled') THEN
        NEW.completed_at := COALESCE(NEW.completed_at, now());
    END IF;
    RETURN NEW;
END;
$$;

-- slot_id retains the quota identity independently of credential rotation or
-- removal. A strict job keeps bounded native provider metadata encrypted under
-- native_source_id; the secret's authenticated plaintext retains member order
-- and exact numeric lexemes, which jsonb would erase.
CREATE TABLE olp.media_jobs (
    id uuid PRIMARY KEY,
    upstream_job_id text,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    provider_id uuid NOT NULL REFERENCES olp.providers,
    provider_model text NOT NULL,
    route_slug text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL DEFAULT 'openai'
        CHECK (surface IN ('openai','anthropic','gemini')),
    state text NOT NULL
        CHECK (state IN ('queued','running','succeeded','failed','cancelled')),
    lifecycle_state text NOT NULL DEFAULT 'active'
        CHECK (lifecycle_state IN
            ('creating','active','create_ambiguous','create_cleanup_pending','delete_pending','deleted')),
    progress_percent numeric(5,2)
        CHECK (progress_percent IS NULL OR (progress_percent >= 0 AND progress_percent <= 100)),
    content_available boolean NOT NULL DEFAULT false,
    expires_at timestamptz,
    error_class text,
    completed_at timestamptz,
    last_polled_at timestamptz,
    reconciliation_error text,
    deleted_at timestamptz,
    etag uuid NOT NULL,
    runtime_generation_id uuid NOT NULL REFERENCES olp.runtime_releases,
    provider_revision_id uuid NOT NULL REFERENCES olp.provider_revisions,
    credential_version_id uuid REFERENCES olp.provider_credentials,
    reconciliation_claim_id uuid,
    reconciliation_claimed_until timestamptz,
    reconciliation_attempts integer NOT NULL DEFAULT 0 CHECK (reconciliation_attempts >= 0),
    next_reconciliation_at timestamptz NOT NULL DEFAULT now(),
    last_reconciliation_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    slot_id uuid NOT NULL,
    strict_contract boolean NOT NULL,
    native_source_id uuid,
    CHECK (((state IN ('succeeded','failed','cancelled')) AND completed_at IS NOT NULL)
        OR ((state IN ('queued','running')) AND completed_at IS NULL)),
    CHECK ((lifecycle_state = 'deleted' AND deleted_at IS NOT NULL)
        OR (lifecycle_state <> 'deleted' AND deleted_at IS NULL)),
    CHECK ((reconciliation_claim_id IS NULL AND reconciliation_claimed_until IS NULL)
        OR (reconciliation_claim_id IS NOT NULL AND reconciliation_claimed_until IS NOT NULL)),
    CHECK (lifecycle_state IN ('creating','create_ambiguous','deleted') OR upstream_job_id IS NOT NULL),
    CONSTRAINT media_jobs_strict_source CHECK
        (NOT strict_contract OR lifecycle_state <> 'active' OR native_source_id IS NOT NULL)
);
CREATE UNIQUE INDEX media_jobs_upstream_unique_idx ON olp.media_jobs
    (provider_id, upstream_job_id) WHERE upstream_job_id IS NOT NULL;
CREATE INDEX media_jobs_api_key_created_idx ON olp.media_jobs
    (api_key_id, created_at DESC, id DESC);
CREATE INDEX media_jobs_created_idx ON olp.media_jobs (created_at DESC, id DESC);
CREATE INDEX media_jobs_state_created_idx ON olp.media_jobs
    (state, created_at DESC, id DESC);
CREATE INDEX media_jobs_provider_live_idx ON olp.media_jobs
    (provider_id, provider_revision_id) WHERE lifecycle_state <> 'deleted';
CREATE INDEX media_jobs_reconciliation_due_idx ON olp.media_jobs
    (next_reconciliation_at, created_at, id) WHERE lifecycle_state <> 'deleted';
CREATE INDEX media_jobs_reconciliation_idx ON olp.media_jobs
    (lifecycle_state, updated_at, id)
    WHERE lifecycle_state NOT IN ('active','deleted');
CREATE TRIGGER media_jobs_lifecycle_guard BEFORE UPDATE ON olp.media_jobs
    FOR EACH ROW EXECUTE FUNCTION olp.enforce_media_job_lifecycle_transition();
CREATE TRIGGER media_jobs_transition_guard BEFORE UPDATE ON olp.media_jobs
    FOR EACH ROW EXECUTE FUNCTION olp.enforce_media_job_transition();

-- Provider-owned resources: files, batches, responses, recoverable
-- continuations and Gemini Interactions. Encrypted payloads live under the
-- same UUID in the secrets store. A retried client submission cannot select
-- another provider or route and perform fresh inference; the owner still
-- authorizes every lookup. A Gemini Interaction's provider ID is encrypted, so
-- its upstream_id is only this row's UUID.
CREATE TABLE olp.provider_resources (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('file', 'batch', 'response', 'continuation', 'strict_response',
                                       'interaction', 'strict_file', 'strict_batch')),
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL REFERENCES olp.providers,
    provider_revision_id uuid NOT NULL,
    route_revision_id uuid NOT NULL,
    slot_id uuid NOT NULL,
    credential_id uuid REFERENCES olp.provider_credentials,
    upstream_id text NOT NULL,
    state text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}',
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    contract_version text,
    parent_id uuid,
    submission_id text,
    UNIQUE(kind,provider_id,upstream_id),
    CHECK (octet_length(upstream_id) BETWEEN 1 AND 512),
    CHECK (octet_length(metadata::text)<=16384),
    CONSTRAINT provider_resources_contract_version_check
        CHECK (contract_version IS NULL OR octet_length(contract_version) BETWEEN 1 AND 128),
    CONSTRAINT provider_resources_continuation_contract_check
        CHECK (kind NOT IN ('continuation', 'strict_response')
               OR (contract_version IS NOT NULL AND expires_at IS NOT NULL)),
    CONSTRAINT provider_resources_submission_check
        CHECK (submission_id IS NULL
               OR (kind = 'continuation' AND octet_length(submission_id) BETWEEN 1 AND 128)),
    CONSTRAINT provider_resources_interaction_contract_check
        CHECK (kind <> 'interaction' OR (
            contract_version IS NOT NULL
            AND contract_version = 'gemini-interaction/v1beta'
            AND expires_at IS NOT NULL
            AND expires_at <= created_at + interval '24 hours'
            AND upstream_id = id::text
            AND submission_id IS NULL
        )),
    CONSTRAINT provider_resources_strict_durable_check
        CHECK (kind NOT IN ('strict_file', 'strict_batch') OR (
            contract_version = 'native-durable-v1'
            AND expires_at IS NOT NULL
            AND expires_at <= created_at + interval '7 days'
            AND submission_id IS NULL
        ))
);
CREATE INDEX provider_resources_owner ON olp.provider_resources(api_key_id,kind,created_at DESC,id DESC);
CREATE UNIQUE INDEX provider_resources_submission_identity
    ON olp.provider_resources(api_key_id, submission_id)
    WHERE submission_id IS NOT NULL;
CREATE INDEX provider_resources_parent
    ON olp.provider_resources(api_key_id, parent_id)
    WHERE parent_id IS NOT NULL;

COMMENT ON COLUMN olp.provider_resources.parent_id IS
    'Immutable logical lineage, checked by the resource owner on creation. It may outlive an expired parent; children contain complete bounded state and do not retain parent payloads by foreign key.';
COMMENT ON COLUMN olp.provider_resources.contract_version IS
    'Version of the resource-owned encrypted interaction contract. Kinds that store an encrypted payload under a contract version, such as strict responses, files and batches, stay separate from the metadata-only kinds of transformed routes.';
COMMENT ON COLUMN olp.provider_resources.submission_id IS
    'Owner-scoped client submission identity for accepted-work and delivery replay. Never an authorization credential.';
