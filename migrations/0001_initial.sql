CREATE TYPE olp_v3.attempt_charge_status AS ENUM (
    'not_billable',
    'billable',
    'billing_uncertain'
);

CREATE TYPE olp_v3.media_job_state AS ENUM (
    'queued',
    'running',
    'succeeded',
    'failed',
    'cancelled'
);

CREATE TYPE olp_v3.provider_state AS ENUM (
    'draft',
    'active',
    'disabled'
);

CREATE TYPE olp_v3.request_metadata_event_receipt_status AS ENUM (
    'pending',
    'fact_persisted',
    'rejected'
);

CREATE TYPE olp_v3.request_metadata_gap_certainty AS ENUM (
    'exact',
    'lower_bound'
);

CREATE TYPE olp_v3.route_draft_state AS ENUM (
    'draft',
    'validated'
);

CREATE TYPE olp_v3.user_role AS ENUM (
    'owner',
    'operator',
    'developer',
    'viewer'
);

CREATE FUNCTION olp_v3.enforce_installation_pricing_currency() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    configured char(3);
BEGIN
    NEW.currency := upper(btrim(NEW.currency));
    INSERT INTO pricing_currency (singleton, currency)
    VALUES (true, NEW.currency)
    ON CONFLICT (singleton) DO NOTHING;

    SELECT currency INTO configured
      FROM pricing_currency
     WHERE singleton
     FOR SHARE;
    IF configured <> NEW.currency THEN
        RAISE EXCEPTION 'pricing currency must match the installation currency %', configured
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION olp_v3.enforce_media_job_lifecycle_transition() RETURNS trigger
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

CREATE FUNCTION olp_v3.enforce_media_job_transition() RETURNS trigger
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

CREATE FUNCTION olp_v3.enforce_oidc_completion_fence() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    checked_etag text;
BEGIN
    checked_etag := current_setting('olp.oidc_configuration_etag', true);
    IF checked_etag IS NULL OR NOT EXISTS (
        SELECT 1
        FROM oidc_configurations
        WHERE singleton
          AND enabled
          AND etag::text = checked_etag
    ) THEN
        RAISE EXCEPTION 'OIDC completion requires a current enabled configuration fence'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION olp_v3.enforce_price_provider_kind() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.provider_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM providers
        WHERE id = NEW.provider_id AND kind = NEW.provider_kind
    ) THEN
        RAISE EXCEPTION 'pricing override provider kind does not match provider'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION olp_v3.enforce_runtime_publication_isolation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'runtime publication requires READ COMMITTED transaction isolation'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION olp_v3.prevent_last_owner_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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

CREATE TABLE olp_v3.api_key_cost_windows (
    api_key_id uuid NOT NULL,
    window_kind text NOT NULL,
    window_id bigint NOT NULL,
    accrued numeric(28,12) NOT NULL,
    unpriced_attempts bigint NOT NULL,
    CONSTRAINT api_key_cost_windows_accrued_check CHECK ((accrued >= (0)::numeric)),
    CONSTRAINT api_key_cost_windows_check CHECK (((window_kind = 'month'::text) OR (unpriced_attempts = 0))),
    CONSTRAINT api_key_cost_windows_unpriced_attempts_check CHECK ((unpriced_attempts >= 0)),
    CONSTRAINT api_key_cost_windows_window_id_check CHECK ((window_id >= 0)),
    CONSTRAINT api_key_cost_windows_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'month'::text])))
);

CREATE TABLE olp_v3.api_key_route_allowlist (
    api_key_id uuid NOT NULL,
    route_slug text NOT NULL
);

CREATE TABLE olp_v3.api_key_scopes (
    api_key_id uuid NOT NULL,
    scope text NOT NULL
);

CREATE TABLE olp_v3.api_keys (
    id uuid NOT NULL,
    lookup_id text NOT NULL,
    secret_digest bytea NOT NULL,
    name text NOT NULL,
    created_by uuid NOT NULL,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    requests_per_minute integer,
    tokens_per_minute bigint,
    max_concurrency integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    etag uuid DEFAULT uuidv7() NOT NULL,
    rotated_at timestamp with time zone,
    daily_cost_limit numeric(24,12),
    monthly_cost_limit numeric(24,12),
    CONSTRAINT api_keys_daily_cost_limit_check CHECK ((daily_cost_limit > (0)::numeric)),
    CONSTRAINT api_keys_lookup_id_check CHECK ((lookup_id ~ '^[A-Za-z0-9_]{8,40}$'::text)),
    CONSTRAINT api_keys_max_concurrency_check CHECK ((max_concurrency > 0)),
    CONSTRAINT api_keys_monthly_cost_limit_check CHECK ((monthly_cost_limit > (0)::numeric)),
    CONSTRAINT api_keys_requests_per_minute_check CHECK ((requests_per_minute > 0)),
    CONSTRAINT api_keys_secret_digest_check CHECK ((octet_length(secret_digest) = 32)),
    CONSTRAINT api_keys_tokens_per_minute_check CHECK ((tokens_per_minute > 0))
);

CREATE TABLE olp_v3.async_media_jobs (
    id uuid NOT NULL,
    upstream_job_id text,
    api_key_id uuid NOT NULL,
    provider_id uuid NOT NULL,
    route_slug text NOT NULL,
    operation text NOT NULL,
    state olp_v3.media_job_state NOT NULL,
    expires_at timestamp with time zone,
    error_class text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    provider_model text NOT NULL,
    surface text DEFAULT 'openai'::text NOT NULL,
    progress_percent numeric(5,2),
    content_available boolean DEFAULT false NOT NULL,
    completed_at timestamp with time zone,
    last_polled_at timestamp with time zone,
    etag uuid DEFAULT uuidv7() NOT NULL,
    lifecycle_state text DEFAULT 'active'::text NOT NULL,
    reconciliation_error text,
    deleted_at timestamp with time zone,
    runtime_generation_id uuid NOT NULL,
    provider_revision_id uuid NOT NULL,
    reconciliation_claim_id uuid,
    reconciliation_claimed_until timestamp with time zone,
    reconciliation_attempts integer DEFAULT 0 NOT NULL,
    next_reconciliation_at timestamp with time zone DEFAULT now() NOT NULL,
    last_reconciliation_at timestamp with time zone,
    CONSTRAINT async_media_jobs_completion_check CHECK ((((state = ANY (ARRAY['succeeded'::olp_v3.media_job_state, 'failed'::olp_v3.media_job_state, 'cancelled'::olp_v3.media_job_state])) AND (completed_at IS NOT NULL)) OR ((state = ANY (ARRAY['queued'::olp_v3.media_job_state, 'running'::olp_v3.media_job_state])) AND (completed_at IS NULL)))),
    CONSTRAINT async_media_jobs_deleted_at_check CHECK ((((lifecycle_state = 'deleted'::text) AND (deleted_at IS NOT NULL)) OR ((lifecycle_state <> 'deleted'::text) AND (deleted_at IS NULL)))),
    CONSTRAINT async_media_jobs_lifecycle_state_check CHECK ((lifecycle_state = ANY (ARRAY['creating'::text, 'active'::text, 'create_ambiguous'::text, 'create_cleanup_pending'::text, 'delete_pending'::text, 'deleted'::text]))),
    CONSTRAINT async_media_jobs_progress_check CHECK (((progress_percent IS NULL) OR ((progress_percent >= (0)::numeric) AND (progress_percent <= (100)::numeric)))),
    CONSTRAINT async_media_jobs_reconciliation_attempts_check CHECK ((reconciliation_attempts >= 0)),
    CONSTRAINT async_media_jobs_reconciliation_claim_check CHECK ((((reconciliation_claim_id IS NULL) AND (reconciliation_claimed_until IS NULL)) OR ((reconciliation_claim_id IS NOT NULL) AND (reconciliation_claimed_until IS NOT NULL)))),
    CONSTRAINT async_media_jobs_surface_check CHECK ((surface = ANY (ARRAY['openai'::text, 'anthropic'::text, 'gemini'::text]))),
    CONSTRAINT async_media_jobs_upstream_binding_check CHECK (((lifecycle_state = ANY (ARRAY['creating'::text, 'create_ambiguous'::text, 'deleted'::text])) OR (upstream_job_id IS NOT NULL)))
);

CREATE TABLE olp_v3.async_worker_counters (
    singleton boolean DEFAULT true NOT NULL,
    request_metadata_reclaimed_total bigint DEFAULT 0 NOT NULL,
    request_metadata_recovered_total bigint DEFAULT 0 NOT NULL,
    request_metadata_duplicates_total bigint DEFAULT 0 CONSTRAINT async_worker_counters_request_metadata_duplicates_tota_not_null NOT NULL,
    request_metadata_processed_total bigint DEFAULT 0 NOT NULL,
    runtime_outbox_attempts_total bigint DEFAULT 0 NOT NULL,
    runtime_outbox_retry_scheduled_total bigint DEFAULT 0 CONSTRAINT async_worker_counters_runtime_outbox_retry_scheduled_t_not_null NOT NULL,
    runtime_outbox_repeated_attempts_total bigint DEFAULT 0 CONSTRAINT async_worker_counters_runtime_outbox_repeated_attempts_not_null NOT NULL,
    runtime_outbox_published_total bigint DEFAULT 0 NOT NULL,
    runtime_outbox_duplicate_publications_total bigint DEFAULT 0 CONSTRAINT async_worker_counters_runtime_outbox_duplicate_publica_not_null NOT NULL,
    runtime_outbox_abandoned_ownership_total bigint DEFAULT 0 CONSTRAINT async_worker_counters_runtime_outbox_abandoned_ownersh_not_null NOT NULL,
    runtime_outbox_abandoned_claims_total bigint DEFAULT 0 CONSTRAINT async_worker_counters_runtime_outbox_abandoned_claims__not_null NOT NULL,
    runtime_outbox_failed_takeovers_total bigint DEFAULT 0 CONSTRAINT async_worker_counters_runtime_outbox_failed_takeovers__not_null NOT NULL,
    CONSTRAINT async_worker_counters_request_metadata_duplicates_total_check CHECK ((request_metadata_duplicates_total >= 0)),
    CONSTRAINT async_worker_counters_request_metadata_processed_total_check CHECK ((request_metadata_processed_total >= 0)),
    CONSTRAINT async_worker_counters_request_metadata_reclaimed_total_check CHECK ((request_metadata_reclaimed_total >= 0)),
    CONSTRAINT async_worker_counters_request_metadata_recovered_total_check CHECK ((request_metadata_recovered_total >= 0)),
    CONSTRAINT async_worker_counters_runtime_outbox_abandoned_claims_tot_check CHECK ((runtime_outbox_abandoned_claims_total >= 0)),
    CONSTRAINT async_worker_counters_runtime_outbox_abandoned_ownership__check CHECK ((runtime_outbox_abandoned_ownership_total >= 0)),
    CONSTRAINT async_worker_counters_runtime_outbox_attempts_total_check CHECK ((runtime_outbox_attempts_total >= 0)),
    CONSTRAINT async_worker_counters_runtime_outbox_duplicate_publicatio_check CHECK ((runtime_outbox_duplicate_publications_total >= 0)),
    CONSTRAINT async_worker_counters_runtime_outbox_failed_takeovers_tot_check CHECK ((runtime_outbox_failed_takeovers_total >= 0)),
    CONSTRAINT async_worker_counters_runtime_outbox_published_total_check CHECK ((runtime_outbox_published_total >= 0)),
    CONSTRAINT async_worker_counters_runtime_outbox_repeated_attempts_to_check CHECK ((runtime_outbox_repeated_attempts_total >= 0)),
    CONSTRAINT async_worker_counters_runtime_outbox_retry_scheduled_tota_check CHECK ((runtime_outbox_retry_scheduled_total >= 0)),
    CONSTRAINT async_worker_counters_singleton_check CHECK (singleton)
);

CREATE TABLE olp_v3.attempt_usage_facts (
    attempt_id uuid NOT NULL,
    event_id uuid NOT NULL,
    request_id uuid NOT NULL,
    request_started_at timestamp with time zone NOT NULL,
    attempt_ordinal smallint NOT NULL,
    api_key_id uuid NOT NULL,
    provider_id uuid NOT NULL,
    route_slug text NOT NULL,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    charge_status olp_v3.attempt_charge_status NOT NULL,
    usage_observed boolean NOT NULL,
    usage_complete boolean NOT NULL,
    input_tokens bigint,
    output_tokens bigint,
    cached_input_tokens bigint,
    media_units numeric(24,6),
    estimated_cost numeric(24,12),
    unpriced boolean NOT NULL,
    pricing_revision_id uuid,
    currency character(3),
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
    CONSTRAINT attempt_usage_facts_attempt_ordinal_check CHECK ((attempt_ordinal > 0)),
    CONSTRAINT attempt_usage_facts_cached_input_tokens_check CHECK (((cached_input_tokens IS NULL) OR (cached_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_check1 CHECK (((charge_status <> 'not_billable'::olp_v3.attempt_charge_status) OR ((NOT usage_observed) AND usage_complete AND (NOT unpriced) AND (estimated_cost IS NULL) AND (pricing_revision_id IS NULL)))),
    CONSTRAINT attempt_usage_facts_check2 CHECK (((charge_status <> 'billing_uncertain'::olp_v3.attempt_charge_status) OR (NOT usage_complete))),
    CONSTRAINT attempt_usage_facts_check3 CHECK (((estimated_cost IS NULL) OR ((charge_status = 'billable'::olp_v3.attempt_charge_status) AND usage_complete AND (NOT unpriced)))),
    CONSTRAINT attempt_usage_facts_currency_check CHECK (((currency IS NULL) OR (((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text)))),
    CONSTRAINT attempt_usage_facts_estimated_cost_check CHECK (((estimated_cost IS NULL) OR (estimated_cost >= (0)::numeric))),
    CONSTRAINT attempt_usage_facts_input_tokens_check CHECK (((input_tokens IS NULL) OR (input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_media_units_check CHECK (((media_units IS NULL) OR (media_units >= (0)::numeric))),
    CONSTRAINT attempt_usage_facts_output_tokens_check CHECK (((output_tokens IS NULL) OR (output_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_surface_check CHECK ((surface = ANY (ARRAY['openai'::text, 'anthropic'::text, 'gemini'::text, 'unknown'::text])))
);

CREATE TABLE olp_v3.attempt_usage_hourly (
    bucket timestamp with time zone NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    api_key_id uuid,
    request_count bigint NOT NULL,
    provider_request_count bigint NOT NULL,
    model_request_count bigint NOT NULL,
    target_request_count bigint NOT NULL,
    input_tokens numeric(30,0) NOT NULL,
    output_tokens numeric(30,0) NOT NULL,
    cached_input_tokens numeric(30,0) NOT NULL,
    media_units numeric(30,6) NOT NULL,
    estimated_cost numeric(30,12),
    request_unpriced_count bigint NOT NULL,
    provider_unpriced_count bigint NOT NULL,
    model_unpriced_count bigint NOT NULL,
    target_unpriced_count bigint NOT NULL,
    request_incomplete_count bigint NOT NULL,
    provider_incomplete_count bigint NOT NULL,
    model_incomplete_count bigint NOT NULL,
    target_incomplete_count bigint NOT NULL,
    currency character(3),
    unpriced_attempt_count bigint DEFAULT 0 NOT NULL,
    CONSTRAINT attempt_usage_hourly_cached_input_tokens_check CHECK ((cached_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_currency_check CHECK (((currency IS NULL) OR (((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text)))),
    CONSTRAINT attempt_usage_hourly_estimated_cost_check CHECK ((estimated_cost >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_input_tokens_check CHECK ((input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_media_units_check CHECK ((media_units >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_model_incomplete_count_check CHECK ((model_incomplete_count >= 0)),
    CONSTRAINT attempt_usage_hourly_model_request_count_check CHECK ((model_request_count >= 0)),
    CONSTRAINT attempt_usage_hourly_model_unpriced_count_check CHECK ((model_unpriced_count >= 0)),
    CONSTRAINT attempt_usage_hourly_output_tokens_check CHECK ((output_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_provider_incomplete_count_check CHECK ((provider_incomplete_count >= 0)),
    CONSTRAINT attempt_usage_hourly_provider_request_count_check CHECK ((provider_request_count >= 0)),
    CONSTRAINT attempt_usage_hourly_provider_unpriced_count_check CHECK ((provider_unpriced_count >= 0)),
    CONSTRAINT attempt_usage_hourly_request_count_check CHECK ((request_count >= 0)),
    CONSTRAINT attempt_usage_hourly_request_incomplete_count_check CHECK ((request_incomplete_count >= 0)),
    CONSTRAINT attempt_usage_hourly_request_unpriced_count_check CHECK ((request_unpriced_count >= 0)),
    CONSTRAINT attempt_usage_hourly_surface_check CHECK ((surface = ANY (ARRAY['openai'::text, 'anthropic'::text, 'gemini'::text, 'unknown'::text]))),
    CONSTRAINT attempt_usage_hourly_target_incomplete_count_check CHECK ((target_incomplete_count >= 0)),
    CONSTRAINT attempt_usage_hourly_target_request_count_check CHECK ((target_request_count >= 0)),
    CONSTRAINT attempt_usage_hourly_target_unpriced_count_check CHECK ((target_unpriced_count >= 0)),
    CONSTRAINT attempt_usage_hourly_unpriced_attempt_count_check CHECK ((unpriced_attempt_count >= 0))
);

CREATE TABLE olp_v3.attempts (
    id uuid NOT NULL,
    request_id uuid NOT NULL,
    request_started_at timestamp with time zone NOT NULL,
    ordinal smallint NOT NULL,
    provider_id uuid NOT NULL,
    upstream_model text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    status_code integer,
    error_class text,
    committed boolean DEFAULT false NOT NULL,
    latency_ms integer,
    first_byte_ms integer,
    CONSTRAINT attempts_first_byte_ms_check CHECK (((first_byte_ms IS NULL) OR (first_byte_ms >= 0))),
    CONSTRAINT attempts_latency_ms_check CHECK (((latency_ms IS NULL) OR (latency_ms >= 0))),
    CONSTRAINT attempts_ordinal_check CHECK ((ordinal > 0)),
    CONSTRAINT attempts_status_code_check CHECK (((status_code IS NULL) OR ((status_code >= 100) AND (status_code <= 599))))
);

CREATE TABLE olp_v3.audit_events (
    id uuid NOT NULL,
    actor_user_id uuid,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text,
    outcome text NOT NULL,
    source_ip inet,
    user_agent_family text,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE olp_v3.idempotency_records (
    id uuid NOT NULL,
    actor_user_id uuid NOT NULL,
    operation text NOT NULL,
    idempotency_key text NOT NULL,
    state text NOT NULL,
    resource_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    request_fingerprint bytea,
    replay_ciphertext bytea,
    replay_nonce bytea,
    replay_key_version integer,
    CONSTRAINT idempotency_in_progress_has_no_replay CHECK (((state <> 'in_progress'::text) OR ((replay_ciphertext IS NULL) AND (replay_nonce IS NULL) AND (replay_key_version IS NULL)))),
    CONSTRAINT idempotency_records_idempotency_key_check CHECK ((idempotency_key ~ '^[A-Za-z0-9._-]{8,128}$'::text)),
    CONSTRAINT idempotency_records_state_check CHECK ((state = ANY (ARRAY['in_progress'::text, 'completed'::text]))),
    CONSTRAINT idempotency_replay_envelope_complete CHECK ((((replay_ciphertext IS NULL) AND (replay_nonce IS NULL) AND (replay_key_version IS NULL)) OR ((replay_ciphertext IS NOT NULL) AND (octet_length(replay_ciphertext) >= 16) AND (replay_nonce IS NOT NULL) AND (octet_length(replay_nonce) = 12) AND (replay_key_version > 0)))),
    CONSTRAINT idempotency_replay_resource_is_encrypted CHECK (((request_fingerprint IS NULL) OR (resource_id IS NULL))),
    CONSTRAINT idempotency_request_fingerprint_size CHECK (((request_fingerprint IS NULL) OR (octet_length(request_fingerprint) = 32)))
);

CREATE TABLE olp_v3.installation (
    singleton boolean DEFAULT true NOT NULL,
    installation_name text CONSTRAINT installation_organization_name_not_null NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT installation_singleton_check CHECK (singleton)
);

CREATE TABLE olp_v3.installation_identity (
    singleton boolean DEFAULT true NOT NULL,
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT installation_identity_singleton_check CHECK (singleton)
);

CREATE TABLE olp_v3.invitations (
    id uuid NOT NULL,
    email text NOT NULL,
    role olp_v3.user_role NOT NULL,
    token_digest bytea NOT NULL,
    invited_by uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    accepted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    accepted_by uuid,
    revoked_at timestamp with time zone,
    revoked_by uuid,
    expired_at timestamp with time zone,
    CONSTRAINT invitations_acceptance_complete CHECK (((accepted_at IS NULL) = (accepted_by IS NULL))),
    CONSTRAINT invitations_email_normalized CHECK ((email = lower(btrim(email)))),
    CONSTRAINT invitations_expiry_after_creation CHECK ((expires_at > created_at)),
    CONSTRAINT invitations_expiry_not_accepted CHECK ((NOT ((accepted_at IS NOT NULL) AND (expired_at IS NOT NULL)))),
    CONSTRAINT invitations_lifecycle_exclusive CHECK ((NOT ((accepted_at IS NOT NULL) AND (revoked_at IS NOT NULL)))),
    CONSTRAINT invitations_revocation_complete CHECK (((revoked_by IS NULL) OR (revoked_at IS NOT NULL))),
    CONSTRAINT invitations_token_digest_check CHECK ((octet_length(token_digest) = 32))
);

CREATE TABLE olp_v3.model_capabilities (
    provider_model_id uuid NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    mode text NOT NULL,
    source text NOT NULL,
    certified_at timestamp with time zone,
    CONSTRAINT model_capabilities_certification_evidence_check CHECK (((source = 'certified'::text) = (certified_at IS NOT NULL))),
    CONSTRAINT model_capabilities_source_check CHECK ((source = ANY (ARRAY['declared'::text, 'certified'::text])))
);

CREATE TABLE olp_v3.oidc_authorization_flows (
    id uuid NOT NULL,
    configuration_id uuid NOT NULL,
    purpose text NOT NULL,
    actor_user_id uuid,
    state_digest bytea NOT NULL,
    browser_binding_digest bytea NOT NULL,
    encrypted_payload bytea NOT NULL,
    payload_nonce bytea NOT NULL,
    payload_key_version integer NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    configuration_etag uuid NOT NULL,
    actor_session_id uuid,
    actor_security_version bigint,
    recent_auth_purpose text,
    recent_auth_resource_id uuid,
    CONSTRAINT oidc_authorization_flows_browser_binding_digest_check CHECK ((octet_length(browser_binding_digest) = 32)),
    CONSTRAINT oidc_authorization_flows_check1 CHECK ((expires_at > created_at)),
    CONSTRAINT oidc_authorization_flows_encrypted_payload_check CHECK ((octet_length(encrypted_payload) >= 16)),
    CONSTRAINT oidc_authorization_flows_payload_key_version_check CHECK ((payload_key_version > 0)),
    CONSTRAINT oidc_authorization_flows_payload_nonce_check CHECK ((octet_length(payload_nonce) = 12)),
    CONSTRAINT oidc_authorization_flows_purpose_check CHECK ((purpose = ANY (ARRAY['login'::text, 'link'::text, 'reauthenticate'::text]))),
    CONSTRAINT oidc_authorization_flows_security_context CHECK ((((purpose = 'login'::text) AND (actor_user_id IS NULL) AND (actor_session_id IS NULL) AND (actor_security_version IS NULL) AND (recent_auth_purpose IS NULL) AND (recent_auth_resource_id IS NULL)) OR ((purpose = 'link'::text) AND (actor_user_id IS NOT NULL) AND (actor_session_id IS NOT NULL) AND (actor_security_version > 0) AND (recent_auth_purpose IS NULL) AND (recent_auth_resource_id IS NULL)) OR ((purpose = 'reauthenticate'::text) AND (actor_user_id IS NOT NULL) AND (actor_session_id IS NOT NULL) AND (actor_security_version > 0) AND (recent_auth_purpose = ANY (ARRAY['password_enrollment'::text, 'oidc_link'::text, 'oidc_unlink'::text])) AND (((recent_auth_purpose = 'oidc_unlink'::text) AND (recent_auth_resource_id IS NOT NULL)) OR ((recent_auth_purpose <> 'oidc_unlink'::text) AND (recent_auth_resource_id IS NULL)))))),
    CONSTRAINT oidc_authorization_flows_state_digest_check CHECK ((octet_length(state_digest) = 32))
);

CREATE TABLE olp_v3.oidc_configurations (
    id uuid NOT NULL,
    issuer text NOT NULL,
    client_id text NOT NULL,
    encrypted_client_secret bytea,
    secret_nonce bytea,
    secret_key_version integer,
    enabled boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    singleton boolean DEFAULT true NOT NULL,
    discovery_url text,
    authorization_endpoint text,
    token_endpoint text,
    jwks_uri text,
    token_endpoint_auth_method text DEFAULT 'client_secret_basic'::text NOT NULL,
    scopes text[] DEFAULT ARRAY['openid'::text, 'email'::text, 'profile'::text] NOT NULL,
    email_claim text DEFAULT 'email'::text NOT NULL,
    groups_claim text DEFAULT 'groups'::text NOT NULL,
    default_role olp_v3.user_role,
    etag uuid DEFAULT gen_random_uuid() NOT NULL,
    updated_by uuid,
    CONSTRAINT oidc_claim_names_valid CHECK (((email_claim ~ '^[A-Za-z0-9_.:-]{1,128}$'::text) AND (groups_claim ~ '^[A-Za-z0-9_.:-]{1,128}$'::text))),
    CONSTRAINT oidc_client_secret_complete CHECK (((num_nonnulls(encrypted_client_secret, secret_nonce, secret_key_version) = ANY (ARRAY[0, 3])) AND ((encrypted_client_secret IS NULL) OR (octet_length(encrypted_client_secret) >= 16)) AND ((secret_nonce IS NULL) OR (octet_length(secret_nonce) = 12)) AND ((secret_key_version IS NULL) OR (secret_key_version > 0)))),
    CONSTRAINT oidc_configurations_singleton_check CHECK (singleton),
    CONSTRAINT oidc_configurations_token_endpoint_auth_method_check CHECK ((token_endpoint_auth_method = ANY (ARRAY['client_secret_basic'::text, 'client_secret_post'::text]))),
    CONSTRAINT oidc_enabled_configuration_complete CHECK (((NOT enabled) OR ((discovery_url IS NOT NULL) AND (authorization_endpoint IS NOT NULL) AND (token_endpoint IS NOT NULL) AND (jwks_uri IS NOT NULL) AND (encrypted_client_secret IS NOT NULL)))),
    CONSTRAINT oidc_endpoint_lengths CHECK ((((char_length(issuer) >= 1) AND (char_length(issuer) <= 2048)) AND ((char_length(client_id) >= 1) AND (char_length(client_id) <= 512)) AND (client_id !~ '[[:cntrl:]]'::text) AND ((discovery_url IS NULL) OR ((char_length(discovery_url) >= 1) AND (char_length(discovery_url) <= 2048))) AND ((authorization_endpoint IS NULL) OR ((char_length(authorization_endpoint) >= 1) AND (char_length(authorization_endpoint) <= 2048))) AND ((token_endpoint IS NULL) OR ((char_length(token_endpoint) >= 1) AND (char_length(token_endpoint) <= 2048))) AND ((jwks_uri IS NULL) OR ((char_length(jwks_uri) >= 1) AND (char_length(jwks_uri) <= 2048))))),
    CONSTRAINT oidc_scopes_valid CHECK ((((cardinality(scopes) >= 1) AND (cardinality(scopes) <= 20)) AND ('openid'::text = ANY (scopes))))
);

CREATE TABLE olp_v3.oidc_email_role_mappings (
    configuration_id uuid NOT NULL,
    email text NOT NULL,
    role olp_v3.user_role NOT NULL,
    CONSTRAINT oidc_email_role_mappings_email_check CHECK (((email = lower(btrim(email))) AND ((char_length(email) >= 3) AND (char_length(email) <= 254)) AND (email !~ '[[:cntrl:]]'::text)))
);

CREATE TABLE olp_v3.oidc_group_role_mappings (
    configuration_id uuid NOT NULL,
    group_name text NOT NULL,
    role olp_v3.user_role NOT NULL,
    CONSTRAINT oidc_group_role_mappings_group_name_check CHECK (((group_name = btrim(group_name)) AND ((char_length(group_name) >= 1) AND (char_length(group_name) <= 256)) AND (group_name !~ '[[:cntrl:]]'::text)))
);

CREATE TABLE olp_v3.oidc_identities (
    issuer text NOT NULL,
    subject text NOT NULL,
    user_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    email_at_link text,
    last_login_at timestamp with time zone,
    id uuid DEFAULT uuidv7() NOT NULL,
    CONSTRAINT oidc_identity_email_length CHECK (((email_at_link IS NULL) OR (((char_length(email_at_link) >= 3) AND (char_length(email_at_link) <= 254)) AND (email_at_link !~ '[[:cntrl:]]'::text)))),
    CONSTRAINT oidc_identity_issuer_length CHECK (((char_length(issuer) >= 1) AND (char_length(issuer) <= 2048))),
    CONSTRAINT oidc_identity_subject_length CHECK (((char_length(subject) >= 1) AND (char_length(subject) <= 255)))
);

CREATE TABLE olp_v3.oidc_login_flow_consumptions (
    flow_id uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT oidc_login_flow_consumptions_check CHECK ((expires_at > consumed_at))
);

CREATE TABLE olp_v3.prices (
    pricing_revision_id uuid NOT NULL,
    provider_kind text NOT NULL,
    model text NOT NULL,
    operation text NOT NULL,
    input_per_million numeric(24,12),
    output_per_million numeric(24,12),
    unit_price numeric(24,12),
    currency character(3) DEFAULT 'USD'::bpchar NOT NULL,
    provider_id uuid,
    cached_input_per_million numeric(24,12),
    CONSTRAINT prices_currency_format_check CHECK ((((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text))),
    CONSTRAINT prices_operation_check CHECK ((operation = ANY (ARRAY['generation'::text, 'embeddings'::text, 'token_count'::text, 'image_generation'::text, 'image_edit'::text, 'image_variation'::text, 'speech'::text, 'transcription'::text, 'video_create'::text, 'video_list'::text, 'video_get'::text, 'video_content'::text, 'video_delete'::text, 'moderation'::text, 'model_list'::text, 'model_get'::text]))),
    CONSTRAINT prices_provider_kind_check CHECK ((provider_kind = ANY (ARRAY['openai'::text, 'anthropic'::text, 'gemini'::text, 'vertex_ai'::text, 'bedrock'::text, 'azure_openai'::text, 'openai_compatible'::text])))
);

CREATE TABLE olp_v3.pricing_currency (
    singleton boolean DEFAULT true NOT NULL,
    currency character(3) NOT NULL,
    CONSTRAINT pricing_currency_currency_check CHECK ((((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text))),
    CONSTRAINT pricing_currency_singleton_check CHECK (singleton)
);

CREATE TABLE olp_v3.pricing_revisions (
    id uuid NOT NULL,
    revision integer NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE olp_v3.provider_credential_versions (
    id uuid NOT NULL,
    provider_id uuid NOT NULL,
    version integer NOT NULL,
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL,
    master_key_version integer NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT provider_credential_versions_ciphertext_check CHECK ((octet_length(ciphertext) >= 16)),
    CONSTRAINT provider_credential_versions_master_key_version_check CHECK ((master_key_version > 0)),
    CONSTRAINT provider_credential_versions_nonce_check CHECK ((octet_length(nonce) = 12)),
    CONSTRAINT provider_credential_versions_version_check CHECK ((version > 0))
);

CREATE TABLE olp_v3.provider_models (
    id uuid NOT NULL,
    provider_id uuid NOT NULL,
    upstream_model text NOT NULL,
    display_name text NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    discovered_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE olp_v3.provider_revision_capabilities (
    provider_revision_model_id uuid CONSTRAINT provider_revision_capabilit_provider_revision_model_id_not_null NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    mode text NOT NULL,
    source text NOT NULL,
    certified_at timestamp with time zone,
    CONSTRAINT provider_revision_capabilities_evidence_check CHECK (((source = 'certified'::text) = (certified_at IS NOT NULL))),
    CONSTRAINT provider_revision_capabilities_source_check CHECK ((source = ANY (ARRAY['declared'::text, 'certified'::text])))
);

CREATE TABLE olp_v3.provider_revision_models (
    id uuid NOT NULL,
    provider_revision_id uuid NOT NULL,
    source_provider_model_id uuid NOT NULL,
    upstream_model text NOT NULL,
    display_name text NOT NULL,
    enabled boolean NOT NULL,
    discovered_at timestamp with time zone
);

CREATE TABLE olp_v3.provider_revisions (
    id uuid NOT NULL,
    provider_id uuid NOT NULL,
    revision integer NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    endpoint text,
    cloud_region text,
    cloud_project text,
    deployment text,
    api_version text,
    auth_mode text NOT NULL,
    connector_ready boolean NOT NULL,
    credential_version_id uuid,
    source_etag uuid NOT NULL,
    activated_by uuid NOT NULL,
    activated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT provider_revisions_revision_check CHECK ((revision > 0))
);

CREATE TABLE olp_v3.providers (
    id uuid NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    state olp_v3.provider_state DEFAULT 'draft'::olp_v3.provider_state NOT NULL,
    endpoint text,
    cloud_region text,
    api_version text,
    auth_mode text NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    active_credential_version_id uuid,
    cloud_project text,
    deployment text,
    connector_ready boolean DEFAULT true NOT NULL,
    last_probe_at timestamp with time zone,
    last_probe_status text,
    last_probe_detail text,
    active_revision_id uuid,
    CONSTRAINT providers_last_probe_detail_check CHECK (((last_probe_detail IS NULL) OR (char_length(last_probe_detail) <= 500))),
    CONSTRAINT providers_last_probe_status_check CHECK (((last_probe_status IS NULL) OR (last_probe_status = ANY (ARRAY['succeeded'::text, 'failed'::text]))))
);

CREATE TABLE olp_v3.public_auth_rate_limits (
    action text NOT NULL,
    scope text NOT NULL,
    key_digest bytea NOT NULL,
    window_started_at timestamp with time zone NOT NULL,
    attempts integer NOT NULL,
    CONSTRAINT public_auth_rate_limits_action_check CHECK ((action = ANY (ARRAY['local_login'::text, 'invitation_acceptance'::text, 'oidc_login'::text]))),
    CONSTRAINT public_auth_rate_limits_attempts_check CHECK ((attempts > 0)),
    CONSTRAINT public_auth_rate_limits_key_digest_check CHECK ((octet_length(key_digest) = 32)),
    CONSTRAINT public_auth_rate_limits_scope_check CHECK ((scope = ANY (ARRAY['global'::text, 'source'::text, 'source_target'::text])))
);

CREATE TABLE olp_v3.request_metadata_consumer_health (
    singleton boolean DEFAULT true NOT NULL,
    pending_events bigint NOT NULL,
    lag_events bigint NOT NULL,
    oldest_pending_at timestamp with time zone,
    checked_at timestamp with time zone NOT NULL,
    CONSTRAINT request_metadata_consumer_health_lag_events_check CHECK ((lag_events >= 0)),
    CONSTRAINT request_metadata_consumer_health_pending_events_check CHECK ((pending_events >= 0)),
    CONSTRAINT request_metadata_consumer_health_singleton_check CHECK (singleton)
);

CREATE TABLE olp_v3.request_metadata_event_receipts (
    event_id uuid NOT NULL,
    request_id uuid NOT NULL,
    event_sha256 bytea NOT NULL,
    status olp_v3.request_metadata_event_receipt_status NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT request_metadata_event_receipts_event_sha256_check CHECK ((octet_length(event_sha256) = 32))
);

CREATE TABLE olp_v3.request_metadata_gap_hourly (
    bucket timestamp with time zone NOT NULL,
    gateway_instance text NOT NULL,
    reason text NOT NULL,
    event_count bigint NOT NULL,
    first_observed_at timestamp with time zone NOT NULL,
    last_observed_at timestamp with time zone NOT NULL,
    uncertain_gap_count bigint DEFAULT 0 NOT NULL,
    CONSTRAINT request_metadata_gap_hourly_bucket_utc_check CHECK ((bucket = (date_trunc('hour'::text, (first_observed_at AT TIME ZONE 'UTC'::text)) AT TIME ZONE 'UTC'::text))),
    CONSTRAINT request_metadata_gap_hourly_check1 CHECK ((last_observed_at >= first_observed_at)),
    CONSTRAINT request_metadata_gap_hourly_check2 CHECK (((event_count > 0) OR (uncertain_gap_count > 0))),
    CONSTRAINT request_metadata_gap_hourly_event_count_check CHECK ((event_count >= 0)),
    CONSTRAINT request_metadata_gap_hourly_uncertain_gap_count_check CHECK ((uncertain_gap_count >= 0))
);

CREATE TABLE olp_v3.request_metadata_gateway_epochs (
    gateway_instance text NOT NULL,
    process_epoch uuid NOT NULL,
    started_at timestamp with time zone NOT NULL,
    accepted bigint NOT NULL,
    persisted bigint NOT NULL,
    dropped bigint NOT NULL,
    abandoned bigint NOT NULL,
    retrying boolean NOT NULL,
    writer_closed boolean NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    gracefully_closed_at timestamp with time zone,
    stale_candidate_at timestamp with time zone,
    stale_detected_at timestamp with time zone,
    acknowledged_at timestamp with time zone,
    acknowledged_by uuid,
    uncertainty_gap_id uuid,
    CONSTRAINT request_metadata_gateway_epochs_abandoned_check CHECK ((abandoned >= 0)),
    CONSTRAINT request_metadata_gateway_epochs_accepted_check CHECK ((accepted >= 0)),
    CONSTRAINT request_metadata_gateway_epochs_check CHECK ((updated_at >= started_at)),
    CONSTRAINT request_metadata_gateway_epochs_check1 CHECK (((gracefully_closed_at IS NULL) OR (gracefully_closed_at >= started_at))),
    CONSTRAINT request_metadata_gateway_epochs_check2 CHECK (((stale_candidate_at IS NULL) OR (stale_candidate_at >= started_at))),
    CONSTRAINT request_metadata_gateway_epochs_check3 CHECK (((stale_detected_at IS NULL) OR (stale_detected_at >= started_at))),
    CONSTRAINT request_metadata_gateway_epochs_check4 CHECK (((acknowledged_at IS NULL) OR ((stale_detected_at IS NOT NULL) AND (acknowledged_at >= stale_detected_at)))),
    CONSTRAINT request_metadata_gateway_epochs_check5 CHECK ((NOT ((gracefully_closed_at IS NOT NULL) AND (stale_detected_at IS NOT NULL)))),
    CONSTRAINT request_metadata_gateway_epochs_dropped_check CHECK ((dropped >= 0)),
    CONSTRAINT request_metadata_gateway_epochs_persisted_check CHECK ((persisted >= 0))
);

CREATE TABLE olp_v3.request_metadata_ingestion_gaps (
    id uuid NOT NULL,
    gateway_instance text NOT NULL,
    event_count bigint NOT NULL,
    reason text NOT NULL,
    first_observed_at timestamp with time zone NOT NULL,
    last_observed_at timestamp with time zone NOT NULL,
    reported_at timestamp with time zone DEFAULT now() NOT NULL,
    certainty olp_v3.request_metadata_gap_certainty DEFAULT 'exact'::olp_v3.request_metadata_gap_certainty NOT NULL,
    deduplication_key text,
    CONSTRAINT request_metadata_ingestion_gaps_check CHECK ((last_observed_at >= first_observed_at)),
    CONSTRAINT request_metadata_ingestion_gaps_check1 CHECK (((certainty = 'lower_bound'::olp_v3.request_metadata_gap_certainty) OR (event_count > 0))),
    CONSTRAINT request_metadata_ingestion_gaps_deduplication_key_check CHECK (((deduplication_key IS NULL) OR ((deduplication_key <> ''::text) AND (octet_length(deduplication_key) <= 256)))),
    CONSTRAINT request_metadata_ingestion_gaps_event_count_check CHECK ((event_count >= 0))
);

CREATE TABLE olp_v3.requests (
    id uuid NOT NULL,
    runtime_generation_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    route_slug text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    status_code integer,
    error_class text,
    total_latency_ms integer,
    first_byte_ms integer,
    attempt_count smallint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT requests_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT requests_first_byte_ms_check CHECK (((first_byte_ms IS NULL) OR (first_byte_ms >= 0))),
    CONSTRAINT requests_status_code_check CHECK (((status_code IS NULL) OR ((status_code >= 100) AND (status_code <= 599)))),
    CONSTRAINT requests_total_latency_ms_check CHECK (((total_latency_ms IS NULL) OR (total_latency_ms >= 0)))
)
PARTITION BY RANGE (started_at);

CREATE TABLE olp_v3.requests_default (
    id uuid CONSTRAINT requests_id_not_null NOT NULL,
    runtime_generation_id uuid CONSTRAINT requests_runtime_generation_id_not_null NOT NULL,
    api_key_id uuid CONSTRAINT requests_api_key_id_not_null NOT NULL,
    route_slug text CONSTRAINT requests_route_slug_not_null NOT NULL,
    operation text CONSTRAINT requests_operation_not_null NOT NULL,
    surface text CONSTRAINT requests_surface_not_null NOT NULL,
    started_at timestamp with time zone CONSTRAINT requests_started_at_not_null NOT NULL,
    completed_at timestamp with time zone,
    status_code integer,
    error_class text,
    total_latency_ms integer,
    first_byte_ms integer,
    attempt_count smallint DEFAULT 0 CONSTRAINT requests_attempt_count_not_null NOT NULL,
    created_at timestamp with time zone DEFAULT now() CONSTRAINT requests_created_at_not_null NOT NULL,
    CONSTRAINT requests_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT requests_first_byte_ms_check CHECK (((first_byte_ms IS NULL) OR (first_byte_ms >= 0))),
    CONSTRAINT requests_status_code_check CHECK (((status_code IS NULL) OR ((status_code >= 100) AND (status_code <= 599)))),
    CONSTRAINT requests_total_latency_ms_check CHECK (((total_latency_ms IS NULL) OR (total_latency_ms >= 0)))
);

CREATE TABLE olp_v3.route_draft_operations (
    route_draft_id uuid NOT NULL,
    operation text NOT NULL
);

CREATE TABLE olp_v3.route_draft_targets (
    id uuid NOT NULL,
    route_draft_id uuid NOT NULL,
    provider_model_id uuid NOT NULL,
    priority integer NOT NULL,
    weight integer NOT NULL,
    timeout_ms integer NOT NULL,
    "position" integer NOT NULL,
    routing_id uuid NOT NULL,
    CONSTRAINT route_draft_targets_position_check CHECK (("position" >= 0)),
    CONSTRAINT route_draft_targets_priority_check CHECK ((priority >= 0)),
    CONSTRAINT route_draft_targets_timeout_ms_check CHECK ((timeout_ms > 0)),
    CONSTRAINT route_draft_targets_weight_check CHECK ((weight > 0))
);

CREATE TABLE olp_v3.route_drafts (
    id uuid NOT NULL,
    slug text NOT NULL,
    state olp_v3.route_draft_state DEFAULT 'draft'::olp_v3.route_draft_state NOT NULL,
    overall_timeout_ms integer NOT NULL,
    max_attempts smallint NOT NULL,
    etag uuid NOT NULL,
    based_on_revision_id uuid,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    routing_id uuid NOT NULL,
    CONSTRAINT route_drafts_max_attempts_check CHECK ((max_attempts > 0)),
    CONSTRAINT route_drafts_overall_timeout_ms_check CHECK ((overall_timeout_ms > 0)),
    CONSTRAINT route_drafts_slug CHECK (((slug ~ '^[a-z0-9]$'::text) OR ((slug ~ '^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$'::text) AND (POSITION(('--'::text) IN (slug)) = 0))))
);

CREATE TABLE olp_v3.route_revision_operations (
    route_revision_id uuid NOT NULL,
    operation text NOT NULL
);

CREATE TABLE olp_v3.route_revision_targets (
    id uuid NOT NULL,
    route_revision_id uuid NOT NULL,
    provider_model_id uuid NOT NULL,
    priority integer NOT NULL,
    weight integer NOT NULL,
    timeout_ms integer NOT NULL,
    "position" integer NOT NULL,
    routing_id uuid NOT NULL,
    CONSTRAINT route_revision_targets_position_check CHECK (("position" >= 0)),
    CONSTRAINT route_revision_targets_priority_check CHECK ((priority >= 0)),
    CONSTRAINT route_revision_targets_timeout_ms_check CHECK ((timeout_ms > 0)),
    CONSTRAINT route_revision_targets_weight_check CHECK ((weight > 0))
);

CREATE TABLE olp_v3.route_revisions (
    id uuid NOT NULL,
    route_id uuid NOT NULL,
    revision integer NOT NULL,
    slug text NOT NULL,
    overall_timeout_ms integer NOT NULL,
    max_attempts smallint NOT NULL,
    source_draft_id uuid NOT NULL,
    activated_by uuid NOT NULL,
    activated_at timestamp with time zone DEFAULT now() NOT NULL,
    routing_id uuid NOT NULL,
    CONSTRAINT route_revisions_max_attempts_check CHECK ((max_attempts > 0)),
    CONSTRAINT route_revisions_overall_timeout_ms_check CHECK ((overall_timeout_ms > 0)),
    CONSTRAINT route_revisions_revision_check CHECK ((revision > 0)),
    CONSTRAINT route_revisions_slug CHECK (((slug ~ '^[a-z0-9]$'::text) OR ((slug ~ '^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$'::text) AND (POSITION(('--'::text) IN (slug)) = 0))))
);

CREATE TABLE olp_v3.routes (
    id uuid NOT NULL,
    slug text NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT routes_slug CHECK (((slug ~ '^[a-z0-9]$'::text) OR ((slug ~ '^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$'::text) AND (POSITION(('--'::text) IN (slug)) = 0))))
);

CREATE TABLE olp_v3.runtime_generation_provider_configs (
    runtime_generation_id uuid CONSTRAINT runtime_generation_provider_conf_runtime_generation_id_not_null NOT NULL,
    provider_id uuid NOT NULL,
    kind text NOT NULL,
    endpoint text,
    cloud_region text,
    cloud_project text,
    deployment text,
    api_version text,
    auth_mode text NOT NULL,
    active_credential_version_id uuid,
    provider_revision_id uuid
);

CREATE TABLE olp_v3.runtime_generations (
    id uuid NOT NULL,
    sequence bigint NOT NULL,
    compiled_release bytea NOT NULL,
    release_sha256 bytea NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT runtime_generations_release_sha256_check CHECK ((octet_length(release_sha256) = 32))
);

ALTER TABLE olp_v3.runtime_generations ALTER COLUMN sequence ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME olp_v3.runtime_generations_sequence_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE olp_v3.runtime_outbox_health (
    singleton boolean DEFAULT true NOT NULL,
    owner_active boolean NOT NULL,
    claimed_rows bigint DEFAULT 0 NOT NULL,
    checked_at timestamp with time zone NOT NULL,
    last_progress_at timestamp with time zone,
    CONSTRAINT runtime_outbox_health_check CHECK ((owner_active OR (claimed_rows = 0))),
    CONSTRAINT runtime_outbox_health_claimed_rows_check CHECK (((claimed_rows >= 0) AND (claimed_rows <= 1))),
    CONSTRAINT runtime_outbox_health_singleton_check CHECK (singleton)
);

CREATE TABLE olp_v3.sessions (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    token_digest bytea NOT NULL,
    csrf_digest bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    security_version bigint NOT NULL,
    recent_auth_token_digest bytea,
    recent_auth_purpose text,
    recent_auth_resource_id uuid,
    recent_auth_expires_at timestamp with time zone,
    CONSTRAINT sessions_csrf_digest_check CHECK ((octet_length(csrf_digest) = 32)),
    CONSTRAINT sessions_recent_auth_complete CHECK ((((recent_auth_token_digest IS NULL) AND (recent_auth_purpose IS NULL) AND (recent_auth_resource_id IS NULL) AND (recent_auth_expires_at IS NULL)) OR ((recent_auth_token_digest IS NOT NULL) AND (octet_length(recent_auth_token_digest) = 32) AND (recent_auth_purpose = ANY (ARRAY['password_enrollment'::text, 'oidc_link'::text, 'oidc_unlink'::text])) AND (recent_auth_expires_at IS NOT NULL) AND (((recent_auth_purpose = 'oidc_unlink'::text) AND (recent_auth_resource_id IS NOT NULL)) OR ((recent_auth_purpose <> 'oidc_unlink'::text) AND (recent_auth_resource_id IS NULL)))))),
    CONSTRAINT sessions_security_version_positive CHECK ((security_version > 0)),
    CONSTRAINT sessions_token_digest_check CHECK ((octet_length(token_digest) = 32))
);

CREATE TABLE olp_v3.settings (
    key text NOT NULL,
    value text NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE olp_v3.transactional_outbox (
    id uuid NOT NULL,
    topic text NOT NULL,
    aggregate_id uuid NOT NULL,
    payload bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    published_at timestamp with time zone,
    publication_attempts bigint DEFAULT 0 NOT NULL,
    CONSTRAINT transactional_outbox_publication_attempts_check CHECK ((publication_attempts >= 0))
);

CREATE TABLE olp_v3.usage_request_anchors (
    request_id uuid NOT NULL,
    request_started_at timestamp with time zone NOT NULL
);

CREATE TABLE olp_v3.users (
    id uuid NOT NULL,
    email text NOT NULL,
    display_name text NOT NULL,
    password_hash text,
    role olp_v3.user_role NOT NULL,
    active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    etag uuid DEFAULT gen_random_uuid() NOT NULL,
    security_version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT users_email_normalized CHECK ((email = lower(btrim(email)))),
    CONSTRAINT users_security_version_check CHECK ((security_version > 0))
);

CREATE TABLE olp_v3.worker_task_health (
    task text NOT NULL,
    checked_at timestamp with time zone NOT NULL,
    last_success_at timestamp with time zone,
    last_progress_at timestamp with time zone,
    successes_total bigint DEFAULT 0 NOT NULL,
    failures_total bigint DEFAULT 0 NOT NULL,
    skipped_total bigint DEFAULT 0 NOT NULL,
    CONSTRAINT worker_task_health_failures_total_check CHECK ((failures_total >= 0)),
    CONSTRAINT worker_task_health_skipped_total_check CHECK ((skipped_total >= 0)),
    CONSTRAINT worker_task_health_successes_total_check CHECK ((successes_total >= 0)),
    CONSTRAINT worker_task_health_task_check CHECK ((task = ANY (ARRAY['runtime_outbox'::text, 'request_metadata_consumer'::text, 'maintenance'::text, 'request_metadata_gateway_epoch_detection'::text, 'cost_reconciliation'::text])))
);

ALTER TABLE ONLY olp_v3.requests ATTACH PARTITION olp_v3.requests_default DEFAULT;

ALTER TABLE ONLY olp_v3.api_key_cost_windows
    ADD CONSTRAINT api_key_cost_windows_pkey PRIMARY KEY (api_key_id, window_kind, window_id);

ALTER TABLE ONLY olp_v3.api_key_route_allowlist
    ADD CONSTRAINT api_key_route_allowlist_pkey PRIMARY KEY (api_key_id, route_slug);

ALTER TABLE ONLY olp_v3.api_key_scopes
    ADD CONSTRAINT api_key_scopes_pkey PRIMARY KEY (api_key_id, scope);

ALTER TABLE ONLY olp_v3.api_keys
    ADD CONSTRAINT api_keys_lookup_id_key UNIQUE (lookup_id);

ALTER TABLE ONLY olp_v3.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.async_media_jobs
    ADD CONSTRAINT async_media_jobs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.async_worker_counters
    ADD CONSTRAINT async_worker_counters_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp_v3.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_pkey PRIMARY KEY (attempt_id);

ALTER TABLE ONLY olp_v3.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_request_id_attempt_ordinal_key UNIQUE (request_id, attempt_ordinal);

ALTER TABLE ONLY olp_v3.attempt_usage_hourly
    ADD CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id);

ALTER TABLE ONLY olp_v3.attempts
    ADD CONSTRAINT attempts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.attempts
    ADD CONSTRAINT attempts_request_id_ordinal_key UNIQUE (request_id, ordinal);

ALTER TABLE ONLY olp_v3.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.idempotency_records
    ADD CONSTRAINT idempotency_records_actor_user_id_operation_idempotency_key_key UNIQUE (actor_user_id, operation, idempotency_key);

ALTER TABLE ONLY olp_v3.idempotency_records
    ADD CONSTRAINT idempotency_records_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.installation_identity
    ADD CONSTRAINT installation_identity_id_key UNIQUE (id);

ALTER TABLE ONLY olp_v3.installation_identity
    ADD CONSTRAINT installation_identity_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp_v3.installation
    ADD CONSTRAINT installation_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp_v3.invitations
    ADD CONSTRAINT invitations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.invitations
    ADD CONSTRAINT invitations_token_digest_key UNIQUE (token_digest);

ALTER TABLE ONLY olp_v3.model_capabilities
    ADD CONSTRAINT model_capabilities_pkey PRIMARY KEY (provider_model_id, operation, surface, mode);

ALTER TABLE ONLY olp_v3.oidc_authorization_flows
    ADD CONSTRAINT oidc_authorization_flows_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.oidc_authorization_flows
    ADD CONSTRAINT oidc_authorization_flows_state_digest_key UNIQUE (state_digest);

ALTER TABLE ONLY olp_v3.oidc_configurations
    ADD CONSTRAINT oidc_configurations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.oidc_email_role_mappings
    ADD CONSTRAINT oidc_email_role_mappings_pkey PRIMARY KEY (configuration_id, email);

ALTER TABLE ONLY olp_v3.oidc_group_role_mappings
    ADD CONSTRAINT oidc_group_role_mappings_pkey PRIMARY KEY (configuration_id, group_name);

ALTER TABLE ONLY olp_v3.oidc_identities
    ADD CONSTRAINT oidc_identities_id_unique UNIQUE (id);

ALTER TABLE ONLY olp_v3.oidc_identities
    ADD CONSTRAINT oidc_identities_pkey PRIMARY KEY (issuer, subject);

ALTER TABLE ONLY olp_v3.oidc_identities
    ADD CONSTRAINT oidc_identities_user_id_issuer_key UNIQUE (user_id, issuer);

ALTER TABLE ONLY olp_v3.oidc_login_flow_consumptions
    ADD CONSTRAINT oidc_login_flow_consumptions_pkey PRIMARY KEY (flow_id);

ALTER TABLE ONLY olp_v3.prices
    ADD CONSTRAINT prices_revision_scope_key UNIQUE NULLS NOT DISTINCT (pricing_revision_id, provider_kind, provider_id, model, operation);

ALTER TABLE ONLY olp_v3.pricing_currency
    ADD CONSTRAINT pricing_currency_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp_v3.pricing_revisions
    ADD CONSTRAINT pricing_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.pricing_revisions
    ADD CONSTRAINT pricing_revisions_revision_key UNIQUE (revision);

ALTER TABLE ONLY olp_v3.provider_credential_versions
    ADD CONSTRAINT provider_credential_owner_key UNIQUE (provider_id, id);

ALTER TABLE ONLY olp_v3.provider_credential_versions
    ADD CONSTRAINT provider_credential_versions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.provider_credential_versions
    ADD CONSTRAINT provider_credential_versions_provider_id_version_key UNIQUE (provider_id, version);

ALTER TABLE ONLY olp_v3.provider_models
    ADD CONSTRAINT provider_models_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.provider_models
    ADD CONSTRAINT provider_models_provider_id_upstream_model_key UNIQUE (provider_id, upstream_model);

ALTER TABLE ONLY olp_v3.provider_revision_capabilities
    ADD CONSTRAINT provider_revision_capabilities_pkey PRIMARY KEY (provider_revision_model_id, operation, surface, mode);

ALTER TABLE ONLY olp_v3.provider_revision_models
    ADD CONSTRAINT provider_revision_models_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.provider_revision_models
    ADD CONSTRAINT provider_revision_models_provider_revision_id_source_provid_key UNIQUE (provider_revision_id, source_provider_model_id);

ALTER TABLE ONLY olp_v3.provider_revision_models
    ADD CONSTRAINT provider_revision_models_provider_revision_id_upstream_mode_key UNIQUE (provider_revision_id, upstream_model);

ALTER TABLE ONLY olp_v3.provider_revisions
    ADD CONSTRAINT provider_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.provider_revisions
    ADD CONSTRAINT provider_revisions_provider_id_id_key UNIQUE (provider_id, id);

ALTER TABLE ONLY olp_v3.provider_revisions
    ADD CONSTRAINT provider_revisions_provider_id_revision_key UNIQUE (provider_id, revision);

ALTER TABLE ONLY olp_v3.providers
    ADD CONSTRAINT providers_name_key UNIQUE (name);

ALTER TABLE ONLY olp_v3.providers
    ADD CONSTRAINT providers_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.public_auth_rate_limits
    ADD CONSTRAINT public_auth_rate_limits_pkey PRIMARY KEY (action, scope, key_digest);

ALTER TABLE ONLY olp_v3.request_metadata_consumer_health
    ADD CONSTRAINT request_metadata_consumer_health_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp_v3.request_metadata_event_receipts
    ADD CONSTRAINT request_metadata_event_receipts_pkey PRIMARY KEY (event_id);

ALTER TABLE ONLY olp_v3.request_metadata_event_receipts
    ADD CONSTRAINT request_metadata_event_receipts_request_id_key UNIQUE (request_id);

ALTER TABLE ONLY olp_v3.request_metadata_gap_hourly
    ADD CONSTRAINT request_metadata_gap_hourly_pkey PRIMARY KEY (bucket, gateway_instance, reason);

ALTER TABLE ONLY olp_v3.request_metadata_gateway_epochs
    ADD CONSTRAINT request_metadata_gateway_epochs_pkey PRIMARY KEY (gateway_instance, process_epoch);

ALTER TABLE ONLY olp_v3.request_metadata_ingestion_gaps
    ADD CONSTRAINT request_metadata_ingestion_gaps_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.requests
    ADD CONSTRAINT requests_pkey PRIMARY KEY (id, started_at);

ALTER TABLE ONLY olp_v3.requests_default
    ADD CONSTRAINT requests_default_pkey PRIMARY KEY (id, started_at);

ALTER TABLE ONLY olp_v3.route_draft_operations
    ADD CONSTRAINT route_draft_operations_pkey PRIMARY KEY (route_draft_id, operation);

ALTER TABLE ONLY olp_v3.route_draft_targets
    ADD CONSTRAINT route_draft_targets_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.route_draft_targets
    ADD CONSTRAINT route_draft_targets_route_draft_id_position_key UNIQUE (route_draft_id, "position");

ALTER TABLE ONLY olp_v3.route_drafts
    ADD CONSTRAINT route_drafts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.route_revision_operations
    ADD CONSTRAINT route_revision_operations_pkey PRIMARY KEY (route_revision_id, operation);

ALTER TABLE ONLY olp_v3.route_revision_targets
    ADD CONSTRAINT route_revision_targets_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.route_revision_targets
    ADD CONSTRAINT route_revision_targets_route_revision_id_position_key UNIQUE (route_revision_id, "position");

ALTER TABLE ONLY olp_v3.route_revisions
    ADD CONSTRAINT route_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.route_revisions
    ADD CONSTRAINT route_revisions_route_id_revision_key UNIQUE (route_id, revision);

ALTER TABLE ONLY olp_v3.routes
    ADD CONSTRAINT routes_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.routes
    ADD CONSTRAINT routes_slug_key UNIQUE (slug);

ALTER TABLE ONLY olp_v3.runtime_generation_provider_configs
    ADD CONSTRAINT runtime_generation_provider_configs_pkey PRIMARY KEY (runtime_generation_id, provider_id);

ALTER TABLE ONLY olp_v3.runtime_generations
    ADD CONSTRAINT runtime_generations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.runtime_generations
    ADD CONSTRAINT runtime_generations_sequence_key UNIQUE (sequence);

ALTER TABLE ONLY olp_v3.runtime_outbox_health
    ADD CONSTRAINT runtime_outbox_health_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp_v3.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.sessions
    ADD CONSTRAINT sessions_token_digest_key UNIQUE (token_digest);

ALTER TABLE ONLY olp_v3.settings
    ADD CONSTRAINT settings_pkey PRIMARY KEY (key);

ALTER TABLE ONLY olp_v3.transactional_outbox
    ADD CONSTRAINT transactional_outbox_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.usage_request_anchors
    ADD CONSTRAINT usage_request_anchors_pkey PRIMARY KEY (request_id, request_started_at);

ALTER TABLE ONLY olp_v3.users
    ADD CONSTRAINT users_email_unique UNIQUE (email);

ALTER TABLE ONLY olp_v3.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp_v3.worker_task_health
    ADD CONSTRAINT worker_task_health_pkey PRIMARY KEY (task);

CREATE INDEX async_media_jobs_api_key_created_idx ON olp_v3.async_media_jobs USING btree (api_key_id, created_at DESC, id DESC);

CREATE INDEX async_media_jobs_created_idx ON olp_v3.async_media_jobs USING btree (created_at DESC, id DESC);

CREATE INDEX async_media_jobs_provider_live_idx ON olp_v3.async_media_jobs USING btree (provider_id, provider_revision_id) WHERE (lifecycle_state <> 'deleted'::text);

CREATE INDEX async_media_jobs_reconciliation_due_idx ON olp_v3.async_media_jobs USING btree (next_reconciliation_at, created_at, id) WHERE (lifecycle_state <> 'deleted'::text);

CREATE INDEX async_media_jobs_reconciliation_idx ON olp_v3.async_media_jobs USING btree (lifecycle_state, updated_at, id) WHERE (lifecycle_state <> ALL (ARRAY['active'::text, 'deleted'::text]));

CREATE INDEX async_media_jobs_state_created_idx ON olp_v3.async_media_jobs USING btree (state, created_at DESC, id DESC);

CREATE UNIQUE INDEX async_media_jobs_upstream_unique_idx ON olp_v3.async_media_jobs USING btree (provider_id, upstream_job_id) WHERE (upstream_job_id IS NOT NULL);

CREATE INDEX attempt_usage_facts_api_key_observed_at_idx ON olp_v3.attempt_usage_facts USING btree (api_key_id, observed_at DESC);

CREATE INDEX attempt_usage_facts_event_id_idx ON olp_v3.attempt_usage_facts USING btree (event_id);

CREATE INDEX attempt_usage_facts_model_idx ON olp_v3.attempt_usage_facts USING btree (upstream_model, observed_at DESC);

CREATE INDEX attempt_usage_facts_observed_at_idx ON olp_v3.attempt_usage_facts USING btree (observed_at DESC);

CREATE INDEX attempt_usage_facts_provider_idx ON olp_v3.attempt_usage_facts USING btree (provider_id, observed_at DESC);

CREATE INDEX attempt_usage_facts_request_idx ON olp_v3.attempt_usage_facts USING btree (request_id, request_started_at, attempt_ordinal);

CREATE INDEX attempt_usage_hourly_api_key_bucket_idx ON olp_v3.attempt_usage_hourly USING btree (api_key_id, bucket DESC) WHERE (api_key_id IS NOT NULL);

CREATE INDEX attempts_provider_started_idx ON olp_v3.attempts USING btree (provider_id, started_at DESC);

CREATE INDEX attempts_request_id_idx ON olp_v3.attempts USING btree (request_id);

CREATE INDEX audit_events_action_occurred_idx ON olp_v3.audit_events USING btree (action, occurred_at DESC, id DESC);

CREATE INDEX audit_events_actor_occurred_idx ON olp_v3.audit_events USING btree (actor_user_id, occurred_at DESC, id DESC);

CREATE INDEX audit_events_occurred_at_idx ON olp_v3.audit_events USING btree (occurred_at DESC);

CREATE INDEX audit_events_resource_occurred_idx ON olp_v3.audit_events USING btree (resource_type, resource_id, occurred_at DESC, id DESC);

CREATE INDEX idempotency_records_expires_at_idx ON olp_v3.idempotency_records USING btree (expires_at);

CREATE UNIQUE INDEX invitations_pending_email_idx ON olp_v3.invitations USING btree (email) WHERE ((accepted_at IS NULL) AND (revoked_at IS NULL) AND (expired_at IS NULL));

CREATE INDEX oidc_authorization_flows_actor_session_idx ON olp_v3.oidc_authorization_flows USING btree (actor_session_id) WHERE (actor_session_id IS NOT NULL);

CREATE INDEX oidc_authorization_flows_expires_at_idx ON olp_v3.oidc_authorization_flows USING btree (expires_at);

CREATE INDEX oidc_identities_user_created_idx ON olp_v3.oidc_identities USING btree (user_id, created_at, id);

CREATE INDEX oidc_login_flow_consumptions_expires_at_idx ON olp_v3.oidc_login_flow_consumptions USING btree (expires_at);

CREATE UNIQUE INDEX oidc_single_configuration_idx ON olp_v3.oidc_configurations USING btree (singleton);

CREATE INDEX prices_provider_override_idx ON olp_v3.prices USING btree (provider_id, model, operation) WHERE (provider_id IS NOT NULL);

CREATE INDEX provider_revision_models_source_idx ON olp_v3.provider_revision_models USING btree (source_provider_model_id, provider_revision_id);

CREATE INDEX provider_revisions_provider_idx ON olp_v3.provider_revisions USING btree (provider_id, revision DESC);

CREATE INDEX public_auth_rate_limits_window_idx ON olp_v3.public_auth_rate_limits USING btree (window_started_at);

CREATE INDEX request_metadata_event_receipts_recorded_at_idx ON olp_v3.request_metadata_event_receipts USING brin (recorded_at);

CREATE INDEX request_metadata_gap_hourly_overlap_idx ON olp_v3.request_metadata_gap_hourly USING btree (last_observed_at, first_observed_at);

CREATE UNIQUE INDEX request_metadata_gateway_epochs_one_open_idx ON olp_v3.request_metadata_gateway_epochs USING btree (gateway_instance) WHERE ((gracefully_closed_at IS NULL) AND (stale_detected_at IS NULL));

CREATE UNIQUE INDEX request_metadata_gateway_epochs_process_epoch_idx ON olp_v3.request_metadata_gateway_epochs USING btree (process_epoch);

CREATE INDEX request_metadata_gateway_epochs_stale_scan_idx ON olp_v3.request_metadata_gateway_epochs USING btree (updated_at) WHERE ((gracefully_closed_at IS NULL) AND (stale_detected_at IS NULL));

CREATE INDEX request_metadata_gateway_epochs_unresolved_idx ON olp_v3.request_metadata_gateway_epochs USING btree (stale_detected_at) WHERE ((stale_detected_at IS NOT NULL) AND (acknowledged_at IS NULL));

CREATE UNIQUE INDEX request_metadata_ingestion_gaps_deduplication_key_idx ON olp_v3.request_metadata_ingestion_gaps USING btree (deduplication_key) WHERE (deduplication_key IS NOT NULL);

CREATE INDEX requests_default_route_idx ON olp_v3.requests_default USING btree (route_slug, started_at DESC);

CREATE INDEX requests_default_started_at_idx ON olp_v3.requests_default USING btree (started_at DESC);

CREATE INDEX route_draft_targets_provider_model_idx ON olp_v3.route_draft_targets USING btree (provider_model_id);

CREATE INDEX route_revision_targets_provider_model_idx ON olp_v3.route_revision_targets USING btree (provider_model_id);

CREATE INDEX route_revisions_route_cursor_idx ON olp_v3.route_revisions USING btree (route_id, revision DESC);

CREATE INDEX runtime_generation_provider_configs_provider_idx ON olp_v3.runtime_generation_provider_configs USING btree (provider_id, runtime_generation_id);

CREATE INDEX sessions_expires_at_idx ON olp_v3.sessions USING btree (expires_at);

CREATE INDEX sessions_recent_auth_expiry_idx ON olp_v3.sessions USING btree (recent_auth_expires_at) WHERE (recent_auth_token_digest IS NOT NULL);

CREATE INDEX sessions_security_version_idx ON olp_v3.sessions USING btree (user_id, security_version);

CREATE INDEX sessions_user_id_idx ON olp_v3.sessions USING btree (user_id);

CREATE INDEX transactional_outbox_pending_order_idx ON olp_v3.transactional_outbox USING btree (created_at, id) WHERE (published_at IS NULL);

CREATE INDEX transactional_outbox_published_idx ON olp_v3.transactional_outbox USING btree (published_at) WHERE (published_at IS NOT NULL);

CREATE INDEX usage_request_anchors_started_at_idx ON olp_v3.usage_request_anchors USING btree (request_started_at);

ALTER INDEX olp_v3.requests_pkey ATTACH PARTITION olp_v3.requests_default_pkey;

CREATE TRIGGER async_media_jobs_lifecycle_guard BEFORE UPDATE ON olp_v3.async_media_jobs FOR EACH ROW EXECUTE FUNCTION olp_v3.enforce_media_job_lifecycle_transition();

CREATE TRIGGER async_media_jobs_transition_guard BEFORE UPDATE ON olp_v3.async_media_jobs FOR EACH ROW EXECUTE FUNCTION olp_v3.enforce_media_job_transition();

CREATE TRIGGER oidc_identities_completion_guard BEFORE INSERT OR UPDATE ON olp_v3.oidc_identities FOR EACH ROW EXECUTE FUNCTION olp_v3.enforce_oidc_completion_fence();

CREATE TRIGGER prices_installation_currency_guard BEFORE INSERT OR UPDATE OF currency ON olp_v3.prices FOR EACH ROW EXECUTE FUNCTION olp_v3.enforce_installation_pricing_currency();

CREATE TRIGGER prices_provider_kind_guard BEFORE INSERT OR UPDATE OF provider_id, provider_kind ON olp_v3.prices FOR EACH ROW EXECUTE FUNCTION olp_v3.enforce_price_provider_kind();

CREATE TRIGGER runtime_generations_isolation_guard BEFORE INSERT ON olp_v3.runtime_generations FOR EACH ROW EXECUTE FUNCTION olp_v3.enforce_runtime_publication_isolation();

CREATE TRIGGER users_last_owner_delete_guard BEFORE DELETE ON olp_v3.users FOR EACH ROW EXECUTE FUNCTION olp_v3.prevent_last_owner_change();

CREATE TRIGGER users_last_owner_update_guard BEFORE UPDATE OF role, active ON olp_v3.users FOR EACH ROW EXECUTE FUNCTION olp_v3.prevent_last_owner_change();

ALTER TABLE ONLY olp_v3.api_key_cost_windows
    ADD CONSTRAINT api_key_cost_windows_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp_v3.api_keys(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.api_key_route_allowlist
    ADD CONSTRAINT api_key_route_allowlist_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp_v3.api_keys(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.api_key_scopes
    ADD CONSTRAINT api_key_scopes_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp_v3.api_keys(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.api_keys
    ADD CONSTRAINT api_keys_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.async_media_jobs
    ADD CONSTRAINT async_media_jobs_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp_v3.api_keys(id);

ALTER TABLE ONLY olp_v3.async_media_jobs
    ADD CONSTRAINT async_media_jobs_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp_v3.providers(id);

ALTER TABLE ONLY olp_v3.async_media_jobs
    ADD CONSTRAINT async_media_jobs_provider_revision_id_fkey FOREIGN KEY (provider_revision_id) REFERENCES olp_v3.provider_revisions(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.async_media_jobs
    ADD CONSTRAINT async_media_jobs_runtime_generation_id_fkey FOREIGN KEY (runtime_generation_id) REFERENCES olp_v3.runtime_generations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp_v3.api_keys(id);

ALTER TABLE ONLY olp_v3.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_pricing_revision_id_fkey FOREIGN KEY (pricing_revision_id) REFERENCES olp_v3.pricing_revisions(id);

ALTER TABLE ONLY olp_v3.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp_v3.providers(id);

ALTER TABLE ONLY olp_v3.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_request_id_request_started_at_fkey FOREIGN KEY (request_id, request_started_at) REFERENCES olp_v3.usage_request_anchors(request_id, request_started_at) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.attempt_usage_hourly
    ADD CONSTRAINT attempt_usage_hourly_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp_v3.api_keys(id);

ALTER TABLE ONLY olp_v3.attempt_usage_hourly
    ADD CONSTRAINT attempt_usage_hourly_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp_v3.providers(id);

ALTER TABLE ONLY olp_v3.attempts
    ADD CONSTRAINT attempts_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp_v3.providers(id);

ALTER TABLE ONLY olp_v3.attempts
    ADD CONSTRAINT attempts_request_id_request_started_at_fkey FOREIGN KEY (request_id, request_started_at) REFERENCES olp_v3.requests(id, started_at) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.audit_events
    ADD CONSTRAINT audit_events_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.idempotency_records
    ADD CONSTRAINT idempotency_records_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES olp_v3.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.invitations
    ADD CONSTRAINT invitations_accepted_by_fkey FOREIGN KEY (accepted_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.invitations
    ADD CONSTRAINT invitations_invited_by_fkey FOREIGN KEY (invited_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.invitations
    ADD CONSTRAINT invitations_revoked_by_fkey FOREIGN KEY (revoked_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.model_capabilities
    ADD CONSTRAINT model_capabilities_provider_model_id_fkey FOREIGN KEY (provider_model_id) REFERENCES olp_v3.provider_models(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.oidc_authorization_flows
    ADD CONSTRAINT oidc_authorization_flows_actor_session_id_fkey FOREIGN KEY (actor_session_id) REFERENCES olp_v3.sessions(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.oidc_authorization_flows
    ADD CONSTRAINT oidc_authorization_flows_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES olp_v3.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.oidc_authorization_flows
    ADD CONSTRAINT oidc_authorization_flows_configuration_id_fkey FOREIGN KEY (configuration_id) REFERENCES olp_v3.oidc_configurations(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.oidc_configurations
    ADD CONSTRAINT oidc_configurations_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.oidc_email_role_mappings
    ADD CONSTRAINT oidc_email_role_mappings_configuration_id_fkey FOREIGN KEY (configuration_id) REFERENCES olp_v3.oidc_configurations(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.oidc_group_role_mappings
    ADD CONSTRAINT oidc_group_role_mappings_configuration_id_fkey FOREIGN KEY (configuration_id) REFERENCES olp_v3.oidc_configurations(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.oidc_identities
    ADD CONSTRAINT oidc_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp_v3.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.prices
    ADD CONSTRAINT prices_pricing_revision_id_fkey FOREIGN KEY (pricing_revision_id) REFERENCES olp_v3.pricing_revisions(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.prices
    ADD CONSTRAINT prices_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp_v3.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.pricing_revisions
    ADD CONSTRAINT pricing_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.provider_credential_versions
    ADD CONSTRAINT provider_credential_versions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.provider_credential_versions
    ADD CONSTRAINT provider_credential_versions_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp_v3.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.provider_models
    ADD CONSTRAINT provider_models_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp_v3.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.provider_revision_capabilities
    ADD CONSTRAINT provider_revision_capabilities_provider_revision_model_id_fkey FOREIGN KEY (provider_revision_model_id) REFERENCES olp_v3.provider_revision_models(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.provider_revision_models
    ADD CONSTRAINT provider_revision_models_provider_revision_id_fkey FOREIGN KEY (provider_revision_id) REFERENCES olp_v3.provider_revisions(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.provider_revision_models
    ADD CONSTRAINT provider_revision_models_source_provider_model_id_fkey FOREIGN KEY (source_provider_model_id) REFERENCES olp_v3.provider_models(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.provider_revisions
    ADD CONSTRAINT provider_revisions_activated_by_fkey FOREIGN KEY (activated_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.provider_revisions
    ADD CONSTRAINT provider_revisions_credential_owner_fk FOREIGN KEY (provider_id, credential_version_id) REFERENCES olp_v3.provider_credential_versions(provider_id, id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp_v3.provider_revisions
    ADD CONSTRAINT provider_revisions_credential_version_id_fkey FOREIGN KEY (credential_version_id) REFERENCES olp_v3.provider_credential_versions(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.provider_revisions
    ADD CONSTRAINT provider_revisions_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp_v3.providers(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.providers
    ADD CONSTRAINT providers_active_credential_owner_fk FOREIGN KEY (id, active_credential_version_id) REFERENCES olp_v3.provider_credential_versions(provider_id, id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp_v3.providers
    ADD CONSTRAINT providers_active_credential_version_id_fkey FOREIGN KEY (active_credential_version_id) REFERENCES olp_v3.provider_credential_versions(id);

ALTER TABLE ONLY olp_v3.providers
    ADD CONSTRAINT providers_active_revision_owner_fk FOREIGN KEY (id, active_revision_id) REFERENCES olp_v3.provider_revisions(provider_id, id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp_v3.providers
    ADD CONSTRAINT providers_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.request_metadata_gateway_epochs
    ADD CONSTRAINT request_metadata_gateway_epochs_acknowledged_by_fkey FOREIGN KEY (acknowledged_by) REFERENCES olp_v3.users(id) ON DELETE SET NULL;

ALTER TABLE ONLY olp_v3.request_metadata_gateway_epochs
    ADD CONSTRAINT request_metadata_gateway_epochs_uncertainty_gap_id_fkey FOREIGN KEY (uncertainty_gap_id) REFERENCES olp_v3.request_metadata_ingestion_gaps(id) ON DELETE SET NULL;

ALTER TABLE olp_v3.requests
    ADD CONSTRAINT requests_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp_v3.api_keys(id);

ALTER TABLE olp_v3.requests
    ADD CONSTRAINT requests_runtime_generation_id_fkey FOREIGN KEY (runtime_generation_id) REFERENCES olp_v3.runtime_generations(id);

ALTER TABLE ONLY olp_v3.route_draft_operations
    ADD CONSTRAINT route_draft_operations_route_draft_id_fkey FOREIGN KEY (route_draft_id) REFERENCES olp_v3.route_drafts(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.route_draft_targets
    ADD CONSTRAINT route_draft_targets_provider_model_id_fkey FOREIGN KEY (provider_model_id) REFERENCES olp_v3.provider_models(id);

ALTER TABLE ONLY olp_v3.route_draft_targets
    ADD CONSTRAINT route_draft_targets_route_draft_id_fkey FOREIGN KEY (route_draft_id) REFERENCES olp_v3.route_drafts(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.route_drafts
    ADD CONSTRAINT route_drafts_based_on_revision_fk FOREIGN KEY (based_on_revision_id) REFERENCES olp_v3.route_revisions(id);

ALTER TABLE ONLY olp_v3.route_drafts
    ADD CONSTRAINT route_drafts_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.route_revision_operations
    ADD CONSTRAINT route_revision_operations_route_revision_id_fkey FOREIGN KEY (route_revision_id) REFERENCES olp_v3.route_revisions(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.route_revision_targets
    ADD CONSTRAINT route_revision_targets_provider_model_id_fkey FOREIGN KEY (provider_model_id) REFERENCES olp_v3.provider_models(id);

ALTER TABLE ONLY olp_v3.route_revision_targets
    ADD CONSTRAINT route_revision_targets_route_revision_id_fkey FOREIGN KEY (route_revision_id) REFERENCES olp_v3.route_revisions(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.route_revisions
    ADD CONSTRAINT route_revisions_activated_by_fkey FOREIGN KEY (activated_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.route_revisions
    ADD CONSTRAINT route_revisions_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp_v3.routes(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.route_revisions
    ADD CONSTRAINT route_revisions_source_draft_id_fkey FOREIGN KEY (source_draft_id) REFERENCES olp_v3.route_drafts(id);

ALTER TABLE ONLY olp_v3.routes
    ADD CONSTRAINT routes_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.runtime_generation_provider_configs
    ADD CONSTRAINT runtime_generation_provider_c_active_credential_version_id_fkey FOREIGN KEY (active_credential_version_id) REFERENCES olp_v3.provider_credential_versions(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.runtime_generation_provider_configs
    ADD CONSTRAINT runtime_generation_provider_configs_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp_v3.providers(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.runtime_generation_provider_configs
    ADD CONSTRAINT runtime_generation_provider_configs_provider_revision_id_fkey FOREIGN KEY (provider_revision_id) REFERENCES olp_v3.provider_revisions(id) ON DELETE RESTRICT;

ALTER TABLE ONLY olp_v3.runtime_generation_provider_configs
    ADD CONSTRAINT runtime_generation_provider_configs_runtime_generation_id_fkey FOREIGN KEY (runtime_generation_id) REFERENCES olp_v3.runtime_generations(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.runtime_generations
    ADD CONSTRAINT runtime_generations_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp_v3.users(id);

ALTER TABLE ONLY olp_v3.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp_v3.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp_v3.settings
    ADD CONSTRAINT settings_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES olp_v3.users(id);

INSERT INTO olp_v3.installation_identity (singleton) VALUES (true);
INSERT INTO olp_v3.async_worker_counters (singleton) VALUES (true);
