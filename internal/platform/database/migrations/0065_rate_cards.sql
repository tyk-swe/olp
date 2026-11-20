-- Discounts: immutable, effective-dated multipliers on provider cost. A scope
-- ends when a new discount at that scope supersedes it; deleting a discount
-- would rewrite what already-settled attempts cost.
CREATE TABLE olp.pricing_discounts (
    id uuid PRIMARY KEY,
    provider_kind text NOT NULL CHECK (provider_kind IN (
        'openai','anthropic','gemini','vertex_ai','bedrock','azure_openai','openai_compatible','plugin','sagemaker','watsonx')),
    vendor_id text,
    provider_id uuid REFERENCES olp.providers ON DELETE CASCADE,
    multiplier numeric(24,12) NOT NULL,
    effective_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid NOT NULL REFERENCES olp.users
);
CREATE INDEX pricing_discounts_scope_effective
    ON olp.pricing_discounts (provider_kind, vendor_id, provider_id, effective_at DESC);

-- Rate cards: one card per owner. An installation card has neither owner key;
-- the unique key must treat NULL equal so an owner gets at most one card.
CREATE TABLE olp.rate_cards (
    id uuid PRIMARY KEY,
    organization_id uuid REFERENCES olp.organizations ON DELETE CASCADE,
    project_id uuid REFERENCES olp.projects ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid NOT NULL REFERENCES olp.users,
    CONSTRAINT rate_cards_owner CHECK (project_id IS NULL OR organization_id IS NULL)
);
CREATE UNIQUE INDEX rate_cards_owner_key
    ON olp.rate_cards (COALESCE(organization_id::text,''), COALESCE(project_id::text,''));

CREATE TABLE olp.rate_card_revisions (
    id uuid PRIMARY KEY,
    rate_card_id uuid NOT NULL REFERENCES olp.rate_cards ON DELETE CASCADE,
    revision integer NOT NULL,
    markup numeric(24,12) NOT NULL,
    effective_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid NOT NULL REFERENCES olp.users,
    UNIQUE (rate_card_id, revision)
);
CREATE INDEX rate_card_revisions_effective
    ON olp.rate_card_revisions (rate_card_id, effective_at DESC);

-- Explicit card rates mirror olp.prices minus provider_id: a card's rates are
-- owner pricing and never target a connection.
CREATE TABLE olp.rate_card_prices (
    rate_card_revision_id uuid NOT NULL REFERENCES olp.rate_card_revisions ON DELETE CASCADE,
    provider_kind text NOT NULL CHECK (provider_kind IN (
        'openai','anthropic','gemini','vertex_ai','bedrock','azure_openai','openai_compatible','plugin','sagemaker','watsonx')),
    vendor_id text,
    model text NOT NULL,
    operation text NOT NULL CHECK (operation ~ '^[a-z][a-z0-9_-]{0,95}$'),
    input_per_million numeric(24,12),
    cached_input_per_million numeric(24,12),
    cache_write_input_per_million numeric(24,12),
    cache_write_5m_input_per_million numeric(24,12),
    cache_write_1h_input_per_million numeric(24,12),
    output_per_million numeric(24,12),
    audio_input_per_million numeric(24,12),
    cached_audio_input_per_million numeric(24,12),
    audio_output_per_million numeric(24,12),
    image_input_per_million numeric(24,12),
    cached_image_input_per_million numeric(24,12),
    image_output_per_million numeric(24,12),
    unit_price numeric(24,12),
    per_request numeric(24,12),
    batch_multiplier numeric(24,12),
    tool_calls jsonb,
    tiers jsonb,
    windows jsonb,
    currency char(3) NOT NULL
        CHECK (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$'),
    CONSTRAINT rate_card_prices_revision_scope_key UNIQUE NULLS NOT DISTINCT
        (rate_card_revision_id, provider_kind, vendor_id, model, operation),
    CHECK (tool_calls IS NULL OR (jsonb_typeof(tool_calls) = 'object'
        AND pg_column_size(tool_calls) <= 8192)),
    CHECK (tiers IS NULL OR (jsonb_typeof(tiers) = 'array'
        AND pg_column_size(tiers) <= 65536)),
    CHECK (windows IS NULL OR (jsonb_typeof(windows) = 'object'
        AND pg_column_size(windows) <= 65536))
);

-- The discount a fact was priced under, and what its consumer was charged.
-- A charge is only recorded for billable attempts with complete usage, the
-- same states that can carry a cost.
ALTER TABLE olp.attempt_usage_facts
    ADD COLUMN discount_id uuid REFERENCES olp.pricing_discounts,
    ADD COLUMN charge numeric(24,12) CHECK (charge IS NULL OR charge >= 0),
    ADD COLUMN rate_card_revision_id uuid REFERENCES olp.rate_card_revisions;
ALTER TABLE olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_charge CHECK (
        (charge IS NULL OR (charge_status <> 'not_billable' AND usage_complete))
        AND (discount_id IS NULL OR estimated_cost IS NOT NULL));

ALTER TABLE olp.attempt_usage_hourly
    ADD COLUMN charge numeric(30,12),
    ADD COLUMN uncharged_attempt_count bigint NOT NULL DEFAULT 0;




CREATE OR REPLACE FUNCTION olp.export_accounted_request() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_request olp.requests%ROWTYPE; v_project uuid; v_outcome text; v_attempt record; v_rollup record;
BEGIN
 IF NEW.status<>'fact_persisted' OR OLD.status='fact_persisted' THEN RETURN NEW; END IF;
 IF NOT EXISTS(SELECT 1 FROM olp.export_sinks WHERE enabled AND streams && ARRAY['requests','attempts','usage_rollups','guardrail_decisions']::text[]) THEN RETURN NEW; END IF;
 SELECT r.* INTO v_request FROM olp.requests r WHERE r.id=NEW.request_id;
 IF NOT FOUND THEN RETURN NEW; END IF;
 SELECT COALESCE(k.project_id,rt.project_id) INTO v_project FROM olp.requests r
 LEFT JOIN olp.api_keys k ON k.id=r.api_key_id LEFT JOIN olp.routes rt ON rt.slug=r.route_slug WHERE r.id=NEW.request_id;
 v_outcome := CASE WHEN v_request.error_class IS NULL AND v_request.status_code BETWEEN 200 AND 299 THEN 'success' ELSE 'failure' END;
 PERFORM olp.enqueue_export('requests',v_request.id,v_project,v_request.route_slug,v_outcome,COALESCE(v_request.completed_at,v_request.started_at),to_jsonb(v_request));
 IF jsonb_array_length(v_request.policy_decisions)>0 THEN
  PERFORM olp.enqueue_export('guardrail_decisions',v_request.id,v_project,v_request.route_slug,v_outcome,COALESCE(v_request.completed_at,v_request.started_at),
   jsonb_build_object('request_id',v_request.id,'decisions',v_request.policy_decisions));
 END IF;
 FOR v_attempt IN SELECT a.* FROM olp.attempts a WHERE a.request_id=NEW.request_id ORDER BY a.ordinal LOOP
  PERFORM olp.enqueue_export('attempts',v_attempt.id,v_project,v_request.route_slug,
   CASE WHEN v_attempt.error_class IS NULL AND v_attempt.status_code BETWEEN 200 AND 299 THEN 'success' ELSE 'failure' END,
   COALESCE(v_attempt.completed_at,v_attempt.started_at),to_jsonb(v_attempt));
 END LOOP;
 FOR v_rollup IN
  SELECT md5(jsonb_build_array(NEW.event_id,f.provider_id,f.route_slug,f.upstream_model,f.operation,f.surface,f.api_key_id,
   f.budget_group_id,f.attribution,f.model_family,f.estimate_provenance,f.end_user_digest,f.budget_exempt,f.currency,
   f.commitment_amount,f.commitment_period,
   date_trunc('hour',f.observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',olp.budget_bucket(f.observed_at))::text)::uuid AS source_id,
   max(f.observed_at) AS observed_at,
   jsonb_build_object('kind','delta','event_id',NEW.event_id,'bucket',date_trunc('hour',f.observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    'budget_bucket',olp.budget_bucket(f.observed_at),'project_id',v_project,'route',f.route_slug,'provider_id',f.provider_id,
    'upstream_model',f.upstream_model,'operation',f.operation,'surface',f.surface,'api_key_id',f.api_key_id,'budget_group_id',f.budget_group_id,
    'model_family',f.model_family,'estimate_provenance',f.estimate_provenance,
    'attribution',f.attribution,'end_user_digest',f.end_user_digest,'budget_exempt',f.budget_exempt,
    'commitment_amount',f.commitment_amount::text,'commitment_period',f.commitment_period,
    'request_count',sum(f.request_counted::integer),'provider_request_count',sum(f.provider_request_counted::integer),
    'model_request_count',sum(f.model_request_counted::integer),'target_request_count',sum(f.target_request_counted::integer),
    'request_unpriced_count',sum(f.request_unpriced_counted::integer),'provider_unpriced_count',sum(f.provider_unpriced_counted::integer),
    'model_unpriced_count',sum(f.model_unpriced_counted::integer),'target_unpriced_count',sum(f.target_unpriced_counted::integer),
    'request_incomplete_count',sum(f.request_incomplete_counted::integer),'provider_incomplete_count',sum(f.provider_incomplete_counted::integer),
    'model_incomplete_count',sum(f.model_incomplete_counted::integer),'target_incomplete_count',sum(f.target_incomplete_counted::integer),
    'attempt_count',count(*),'input_tokens',sum(f.input_tokens),'output_tokens',sum(f.output_tokens),
    'cached_input_tokens',sum(f.cached_input_tokens),'cache_write_input_tokens',sum(f.cache_write_input_tokens),
    'cache_write_5m_input_tokens',sum(f.cache_write_5m_input_tokens),'cache_write_1h_input_tokens',sum(f.cache_write_1h_input_tokens),
    'audio_input_tokens',sum(f.audio_input_tokens),'cached_audio_input_tokens',sum(f.cached_audio_input_tokens),
    'audio_output_tokens',sum(f.audio_output_tokens),
    'image_input_tokens',sum(f.image_input_tokens),'cached_image_input_tokens',sum(f.cached_image_input_tokens),
    'image_output_tokens',sum(f.image_output_tokens),
    'media_units',sum(f.media_units)::text,'estimated_cost',sum(f.estimated_cost)::text,'currency',f.currency,
    'commitment_weight',sum(f.commitment_weight)::text) ||
   jsonb_build_object('charge',sum(f.charge)::text,
    'uncharged_attempt_count',count(*) FILTER(WHERE f.charge_status<>'not_billable' AND f.charge IS NULL),
    'estimated_input_tokens',sum(CASE WHEN f.usage_observed AND f.input_tokens IS NOT NULL AND f.estimated_input_tokens IS NOT NULL THEN f.estimated_input_tokens ELSE 0 END)::text,
    'estimate_reported_input_tokens',sum(CASE WHEN f.usage_observed AND f.input_tokens IS NOT NULL AND f.estimated_input_tokens IS NOT NULL THEN f.input_tokens ELSE 0 END)::text,
    'estimate_attempt_count',sum(CASE WHEN f.usage_observed AND f.input_tokens IS NOT NULL AND f.estimated_input_tokens IS NOT NULL THEN 1 ELSE 0 END),
    'unpriced_attempt_count',count(*) FILTER(WHERE f.charge_status<>'not_billable' AND f.unpriced),
    'incomplete_attempt_count',count(*) FILTER(WHERE f.charge_status<>'not_billable' AND NOT f.usage_complete)) AS payload
  FROM olp.attempt_usage_facts f WHERE f.event_id=NEW.event_id
  GROUP BY f.provider_id,f.route_slug,f.upstream_model,f.operation,f.surface,f.api_key_id,f.budget_group_id,f.attribution,
   f.model_family,f.estimate_provenance,f.end_user_digest,f.budget_exempt,f.currency,f.commitment_amount,f.commitment_period,
   date_trunc('hour',f.observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',olp.budget_bucket(f.observed_at)
 LOOP
  PERFORM olp.enqueue_export('usage_rollups',v_rollup.source_id,v_project,v_request.route_slug,v_outcome,v_rollup.observed_at,v_rollup.payload);
 END LOOP;
 RETURN NEW;
END $$;
