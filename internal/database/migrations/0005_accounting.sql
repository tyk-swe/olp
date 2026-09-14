-- Durable accounting: request history, attempt usage facts, pricing revisions,
-- spend windows, and the health rows the recovery workers checkpoint into.
-- Money never leaves the database as a float: accrued spend, rates, and rolled
-- up aggregates are exact `numeric`, and enum-shaped columns are text with a
-- CHECK so forward migrations stay ordinary DDL.
-- Spend history outlives the key that produced it: the api_key_id references of
-- requests, attempt_usage_facts and attempt_usage_hourly carry no ON DELETE
-- clause, so removing a key is refused rather than silently erasing what it was
-- charged for. Only api_key_cost_windows cascades, because it is a cache the
-- reconciliation pass rebuilds from those facts.

-- Reconstructed spend per budget window. The windows are derived (day number
-- and year*12+month), so a reconciliation pass can rebuild Valkey counters
-- after an outage without replaying facts.
CREATE TABLE olp_go.api_key_cost_windows (
    api_key_id uuid NOT NULL REFERENCES olp_go.api_keys ON DELETE CASCADE,
    window_kind text NOT NULL CHECK (window_kind IN ('day','month')),
    window_id bigint NOT NULL CHECK (window_id >= 0),
    accrued numeric(28,12) NOT NULL CHECK (accrued >= 0),
    unpriced_attempts bigint NOT NULL CHECK (unpriced_attempts >= 0),
    CONSTRAINT api_key_cost_windows_unpriced_scope_check
        CHECK (window_kind = 'month' OR unpriced_attempts = 0),
    PRIMARY KEY (api_key_id, window_kind, window_id)
);

-- Request history is range partitioned so retention purges whole ranges as the
-- installation grows. A DEFAULT partition keeps the schema complete without a
-- partition maintenance worker.
CREATE TABLE olp_go.requests (
    id uuid NOT NULL,
    runtime_generation_id uuid NOT NULL,
    api_key_id uuid NOT NULL REFERENCES olp_go.api_keys,
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
    PRIMARY KEY (id, started_at)
) PARTITION BY RANGE (started_at);
CREATE TABLE olp_go.requests_default PARTITION OF olp_go.requests DEFAULT;
CREATE INDEX requests_route_idx ON olp_go.requests (route_slug, started_at DESC);
CREATE INDEX requests_started_at_idx ON olp_go.requests (started_at DESC);

CREATE TABLE olp_go.attempts (
    id uuid PRIMARY KEY,
    request_id uuid NOT NULL,
    request_started_at timestamptz NOT NULL,
    ordinal smallint NOT NULL CHECK (ordinal > 0),
    provider_id uuid NOT NULL REFERENCES olp_go.providers,
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
        REFERENCES olp_go.requests (id, started_at) ON DELETE CASCADE
);
CREATE INDEX attempts_provider_started_idx ON olp_go.attempts (provider_id, started_at DESC);
CREATE INDEX attempts_request_id_idx ON olp_go.attempts (request_id);

-- Usage facts outlive their request rows: retention purges history earlier than
-- aggregates. The anchor carries the partition key so facts keep a foreign key
-- without pinning the partitioned history table.
CREATE TABLE olp_go.usage_request_anchors (
    request_id uuid NOT NULL,
    request_started_at timestamptz NOT NULL,
    PRIMARY KEY (request_id, request_started_at)
);
CREATE INDEX usage_request_anchors_started_at_idx ON olp_go.usage_request_anchors (request_started_at);

CREATE TABLE olp_go.pricing_revisions (
    id uuid PRIMARY KEY,
    revision integer NOT NULL UNIQUE,
    effective_at timestamptz NOT NULL,
    created_by uuid NOT NULL REFERENCES olp_go.users,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE olp_go.prices (
    pricing_revision_id uuid NOT NULL REFERENCES olp_go.pricing_revisions ON DELETE CASCADE,
    provider_kind text NOT NULL CHECK (provider_kind IN (
        'openai','anthropic','gemini','vertex_ai','bedrock','azure_openai','openai_compatible')),
    model text NOT NULL,
    operation text NOT NULL CHECK (operation IN (
        'generation','embeddings','token_count','image_generation','image_edit','image_variation',
        'speech','transcription','video_create','video_list','video_get','video_content',
        'video_delete','moderation','model_list','model_get')),
    input_per_million numeric(24,12),
    output_per_million numeric(24,12),
    cached_input_per_million numeric(24,12),
    unit_price numeric(24,12),
    currency char(3) NOT NULL DEFAULT 'USD'
        CHECK (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$'),
    provider_id uuid REFERENCES olp_go.providers ON DELETE CASCADE,
    -- Vendor catalogue identity (an `openai_compatible` connector fronting a
    -- known vendor prices against that vendor, not the connector kind).
    vendor_id text,
    CONSTRAINT prices_revision_scope_key UNIQUE NULLS NOT DISTINCT
        (pricing_revision_id, provider_kind, provider_id, vendor_id, model, operation)
);
CREATE INDEX prices_provider_override_idx ON olp_go.prices (provider_id, model, operation)
    WHERE provider_id IS NOT NULL;

-- One installation reports one currency; mixing them would make every total a
-- lie. The first revision sets it and later revisions are checked against it.
CREATE TABLE olp_go.pricing_currency (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    currency char(3) NOT NULL CHECK (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$')
);

-- One row per provider attempt that produced billable evidence. The CHECK
-- constraints encode the charge state machine so no code path can persist a
-- priced row without complete usage, or a cost on a not-billable attempt.
CREATE TABLE olp_go.attempt_usage_facts (
    attempt_id uuid PRIMARY KEY,
    event_id uuid NOT NULL,
    request_id uuid NOT NULL,
    request_started_at timestamptz NOT NULL,
    attempt_ordinal smallint NOT NULL CHECK (attempt_ordinal > 0),
    api_key_id uuid NOT NULL REFERENCES olp_go.api_keys,
    provider_id uuid NOT NULL REFERENCES olp_go.providers,
    route_slug text NOT NULL,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL CHECK (surface IN ('openai','anthropic','gemini','unknown')),
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
    pricing_revision_id uuid REFERENCES olp_go.pricing_revisions,
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
    UNIQUE (request_id, attempt_ordinal),
    FOREIGN KEY (request_id, request_started_at)
        REFERENCES olp_go.usage_request_anchors (request_id, request_started_at) ON DELETE CASCADE,
    CONSTRAINT attempt_usage_facts_not_billable_check CHECK (
        charge_status <> 'not_billable'
        OR (NOT usage_observed AND usage_complete AND NOT unpriced
            AND estimated_cost IS NULL AND pricing_revision_id IS NULL)),
    CONSTRAINT attempt_usage_facts_billing_uncertain_check CHECK (
        charge_status <> 'billing_uncertain' OR NOT usage_complete),
    CONSTRAINT attempt_usage_facts_priced_check CHECK (
        estimated_cost IS NULL
        OR (charge_status = 'billable' AND usage_complete AND NOT unpriced))
);
CREATE INDEX attempt_usage_facts_api_key_observed_at_idx
    ON olp_go.attempt_usage_facts (api_key_id, observed_at DESC);
CREATE INDEX attempt_usage_facts_event_id_idx ON olp_go.attempt_usage_facts (event_id);
CREATE INDEX attempt_usage_facts_model_idx
    ON olp_go.attempt_usage_facts (upstream_model, observed_at DESC);
CREATE INDEX attempt_usage_facts_observed_at_idx ON olp_go.attempt_usage_facts (observed_at DESC);
CREATE INDEX attempt_usage_facts_provider_idx
    ON olp_go.attempt_usage_facts (provider_id, observed_at DESC);
CREATE INDEX attempt_usage_facts_request_idx
    ON olp_go.attempt_usage_facts (request_id, request_started_at, attempt_ordinal);

-- Rolled up usage. Reports read facts and hourly rows as one set, so every
-- count scope of the fact table survives the rollup as a separate column.
CREATE TABLE olp_go.attempt_usage_hourly (
    bucket timestamptz NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL REFERENCES olp_go.providers,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL CHECK (surface IN ('openai','anthropic','gemini','unknown')),
    api_key_id uuid REFERENCES olp_go.api_keys,
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
    -- Named so the rollup can target it by name; api_key_id is nullable and a
    -- NULL key must still collide with the row it rolled into.
    CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
        (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id)
);
CREATE INDEX attempt_usage_hourly_api_key_bucket_idx
    ON olp_go.attempt_usage_hourly (api_key_id, bucket DESC) WHERE api_key_id IS NOT NULL;

-- Gaps are the honest record of metadata that was lost or could not be
-- attributed. Reports treat any overlapping gap as evidence of incompleteness.
CREATE TABLE olp_go.request_metadata_ingestion_gaps (
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
    ON olp_go.request_metadata_ingestion_gaps (deduplication_key) WHERE deduplication_key IS NOT NULL;

CREATE TABLE olp_go.request_metadata_gap_hourly (
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
    ON olp_go.request_metadata_gap_hourly (last_observed_at, first_observed_at);

-- One row per gateway process lifetime. An open epoch that stops checkpointing
-- is detected, marked unclean, and carries the gap that bounds its loss.
CREATE TABLE olp_go.request_metadata_gateway_epochs (
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
    acknowledged_by uuid REFERENCES olp_go.users ON DELETE SET NULL,
    uncertainty_gap_id uuid REFERENCES olp_go.request_metadata_ingestion_gaps ON DELETE SET NULL,
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
    ON olp_go.request_metadata_gateway_epochs (process_epoch);
CREATE UNIQUE INDEX request_metadata_gateway_epochs_one_open_idx
    ON olp_go.request_metadata_gateway_epochs (gateway_instance)
    WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL;
CREATE INDEX request_metadata_gateway_epochs_stale_scan_idx
    ON olp_go.request_metadata_gateway_epochs (updated_at)
    WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL;
CREATE INDEX request_metadata_gateway_epochs_unresolved_idx
    ON olp_go.request_metadata_gateway_epochs (stale_detected_at)
    WHERE stale_detected_at IS NOT NULL AND acknowledged_at IS NULL;

-- Bounded idempotency for stream delivery. A receipt admits one event once
-- within the supported replay window; older deliveries are rejected explicitly
-- rather than silently added to an aggregate they can no longer belong to.
CREATE TABLE olp_go.request_metadata_event_receipts (
    event_id uuid NOT NULL,
    request_id uuid NOT NULL,
    event_sha256 bytea NOT NULL CHECK (octet_length(event_sha256) = 32),
    status text NOT NULL CHECK (status IN ('pending','fact_persisted','rejected')),
    observed_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, request_id)
);
CREATE INDEX request_metadata_event_receipts_recorded_at_idx
    ON olp_go.request_metadata_event_receipts USING brin (recorded_at);

CREATE TABLE olp_go.request_metadata_consumer_health (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    pending_events bigint NOT NULL CHECK (pending_events >= 0),
    lag_events bigint NOT NULL CHECK (lag_events >= 0),
    oldest_pending_at timestamptz,
    checked_at timestamptz NOT NULL
);

-- Fixed worker responsibilities checkpoint here; readiness reads staleness from
-- `checked_at` and progress from `last_progress_at`.
CREATE TABLE olp_go.worker_task_health (
    task text PRIMARY KEY CHECK (task IN (
        'request_metadata_consumer','maintenance','cost_reconciliation',
        'request_metadata_gateway_epoch_detection')),
    checked_at timestamptz NOT NULL,
    last_success_at timestamptz,
    last_progress_at timestamptz,
    successes_total bigint NOT NULL DEFAULT 0 CHECK (successes_total >= 0),
    failures_total bigint NOT NULL DEFAULT 0 CHECK (failures_total >= 0),
    skipped_total bigint NOT NULL DEFAULT 0 CHECK (skipped_total >= 0)
);

CREATE TABLE olp_go.async_worker_counters (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    request_metadata_reclaimed_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_reclaimed_total >= 0),
    request_metadata_recovered_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_recovered_total >= 0),
    request_metadata_duplicates_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_duplicates_total >= 0),
    request_metadata_processed_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_processed_total >= 0)
);
INSERT INTO olp_go.async_worker_counters (singleton) VALUES (true);
