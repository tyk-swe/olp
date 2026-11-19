-- Retained paid work owns a liability independently of native lifecycle state.
-- Resource tombstones and immutable accounting pins survive expiry and deletion.
CREATE TABLE olp.retained_settlements (
 resource_id uuid PRIMARY KEY REFERENCES olp.provider_resources(id),
 operation text NOT NULL,
 templates jsonb NOT NULL CHECK(jsonb_typeof(templates)='object'),
 token_consuming boolean NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','evidence_missing','closed')),
 started_at timestamptz NOT NULL DEFAULT now(),
 remote_terminated boolean NOT NULL DEFAULT false,
 terminal_at timestamptz,
 poll_after timestamptz NOT NULL DEFAULT now(),
 claim_id uuid, lease_until timestamptz,
 failures integer NOT NULL DEFAULT 0,
 last_error text,
 reconciliation_hint text,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX retained_settlements_due ON olp.retained_settlements(poll_after,resource_id) WHERE state<>'closed';
CREATE TABLE olp.retained_mutations (
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 operation text NOT NULL,
 submission_key text NOT NULL CHECK(octet_length(submission_key) BETWEEN 1 AND 128),
 resource_id uuid NOT NULL REFERENCES olp.provider_resources(id),
 request_digest bytea NOT NULL CHECK(octet_length(request_digest)=32),
 state text NOT NULL CHECK(state IN ('prepared','dispatched','accepted','submission_unknown','not_accepted')),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(api_key_id,operation,submission_key)
);
CREATE TABLE olp.retained_usage_evidence (
 id uuid PRIMARY KEY,
 resource_id uuid NOT NULL REFERENCES olp.retained_settlements(resource_id),
 idempotency_key text NOT NULL CHECK(octet_length(idempotency_key) BETWEEN 1 AND 128),
 digest text NOT NULL CHECK(digest ~ '^[a-f0-9]{64}$'),
 provenance text NOT NULL CHECK(provenance IN ('provider_verified','operator_verified')),
 actor text,
 document jsonb NOT NULL,
 remote_termination_verified boolean NOT NULL,
 verified_terminal_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(resource_id,idempotency_key)
);
CREATE TABLE olp.retained_usage_coverage (
 resource_id uuid NOT NULL REFERENCES olp.retained_settlements(resource_id),
 evidence_id uuid NOT NULL REFERENCES olp.retained_usage_evidence,
 component text NOT NULL CHECK(component IN ('training','storage','cache_storage','container','generation')),
 model text NOT NULL,
 memory_tier text NOT NULL DEFAULT '',
 starts_at timestamptz NOT NULL,
 ends_at timestamptz NOT NULL CHECK(ends_at>starts_at),
 quantities jsonb NOT NULL,
 event_id uuid NOT NULL,
 PRIMARY KEY(resource_id,component,model,memory_tier,starts_at,ends_at)
);
CREATE INDEX retained_usage_coverage_intervals ON olp.retained_usage_coverage(resource_id,component,model,memory_tier,starts_at,ends_at);

ALTER TABLE olp.prices ADD COLUMN training_per_million numeric(24,12) CHECK(training_per_million>=0),
 ADD COLUMN storage_per_gib_day numeric(24,12) CHECK(storage_per_gib_day>=0),
 ADD COLUMN cache_storage_per_million_token_hour numeric(24,12) CHECK(cache_storage_per_million_token_hour>=0),
 ADD COLUMN container_per_session jsonb;
ALTER TABLE olp.rate_card_prices ADD COLUMN training_per_million numeric(24,12) CHECK(training_per_million>=0),
 ADD COLUMN storage_per_gib_day numeric(24,12) CHECK(storage_per_gib_day>=0),
 ADD COLUMN cache_storage_per_million_token_hour numeric(24,12) CHECK(cache_storage_per_million_token_hour>=0),
 ADD COLUMN container_per_session jsonb;
ALTER TABLE olp.attempt_usage_facts ADD COLUMN training_tokens bigint CHECK(training_tokens>=0),
 ADD COLUMN storage_byte_seconds numeric CHECK(storage_byte_seconds>=0),
 ADD COLUMN cache_token_seconds numeric CHECK(cache_token_seconds>=0),
 ADD COLUMN container_sessions jsonb;

ALTER TABLE olp.attempt_usage_facts ADD COLUMN invocation_count bigint CHECK(invocation_count>=0);

-- The same indexed projection covers budgets introduced after acceptance.
CREATE FUNCTION olp.project_retained_liability() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE pr olp.provider_resources; template jsonb; v_ordinal integer:=0;
BEGIN
 DELETE FROM olp.provider_resource_pending_usage WHERE resource_id=NEW.resource_id;
 IF NEW.state='closed' THEN RETURN NEW; END IF;
 SELECT * INTO pr FROM olp.provider_resources WHERE id=NEW.resource_id;
 FOR template IN SELECT value FROM jsonb_each(NEW.templates) LOOP
  v_ordinal:=v_ordinal+1;
  pr.kind:='response';
  pr.metadata:=jsonb_build_object('pending_usage',template);
  INSERT INTO olp.provider_resource_pending_usage
  SELECT resource_id,v_ordinal,api_key_id,budget_group_id,project_id,api_key_project_id,route_id,leg_route_id,pinned_route_id,budget_owner_ids,route_slug,provider_id,slot_id,attribution,end_user_digest
   FROM olp.pending_resource_subjects(pr);
 END LOOP;
 RETURN NEW;
END $$;
CREATE TRIGGER retained_liability_projection AFTER INSERT OR UPDATE OF state,templates ON olp.retained_settlements
 FOR EACH ROW EXECUTE FUNCTION olp.project_retained_liability();

-- A resource metadata update must not remove its separate settlement owner.
CREATE OR REPLACE FUNCTION olp.project_pending_resource_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM olp.retained_settlements WHERE resource_id=NEW.id) THEN RETURN NEW; END IF;
 IF NOT olp.resource_owns_pending_usage(NEW.kind) THEN
  DELETE FROM olp.provider_resource_pending_usage WHERE resource_id=NEW.id; RETURN NEW;
 END IF;
 IF TG_OP='UPDATE' AND NEW.kind IS NOT DISTINCT FROM OLD.kind AND NEW.metadata->'pending_usage' IS NOT DISTINCT FROM OLD.metadata->'pending_usage' THEN RETURN NEW; END IF;
 DELETE FROM olp.provider_resource_pending_usage WHERE resource_id=NEW.id;
 IF NEW.metadata->'pending_usage' IS NOT NULL AND NEW.metadata->'pending_usage'<>'null'::jsonb THEN
  INSERT INTO olp.provider_resource_pending_usage SELECT * FROM olp.pending_resource_subjects(NEW);
 END IF;
 RETURN NEW;
END $$;

CREATE VIEW olp.provider_resource_pending_tokens AS SELECT p.* FROM olp.provider_resource_pending_usage p
 LEFT JOIN olp.retained_settlements s ON s.resource_id=p.resource_id WHERE s.resource_id IS NULL OR s.token_consuming;
CREATE VIEW olp.pending_token_recorded_owners AS SELECT o.* FROM olp.pending_resource_recorded_owners o
 JOIN olp.provider_resource_pending_tokens p USING(resource_id,ordinal);

CREATE OR REPLACE FUNCTION olp.has_pending_resource_usage(owner uuid)
RETURNS boolean LANGUAGE plpgsql STABLE AS $$
DECLARE
    pending boolean;
    a olp.aggregate_budget_accounts;
    e olp.end_user_accounts;
    projects uuid[];
    column_name text;
BEGIN
    SELECT EXISTS(SELECT 1 FROM olp.api_keys k JOIN olp.provider_resource_pending_usage p ON p.api_key_id=k.id WHERE k.budget_group_id=owner)
     OR EXISTS(SELECT 1 FROM olp.api_keys k JOIN olp.batch_pending_subjects p ON p.api_key_id=k.id WHERE k.budget_group_id=owner) INTO pending;
    IF pending THEN RETURN true; END IF;
    FOREACH column_name IN ARRAY ARRAY[
        'api_key_id', 'budget_group_id', 'provider_id', 'slot_id',
        'route_id', 'pinned_route_id', 'leg_route_id'
    ] LOOP
        EXECUTE format(
            'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE %1$I=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE %1$I=$1)',
            column_name
        ) INTO pending USING owner;
        IF pending THEN RETURN true; END IF;
    END LOOP;
    EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.pending_resource_recorded_owners WHERE budget_owner_id=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_recorded_owners WHERE budget_owner_id=$1)'
        INTO pending USING owner;
    IF pending THEN RETURN true; END IF;
    SELECT * INTO a FROM olp.aggregate_budget_accounts WHERE id = owner;
    IF FOUND THEN
        IF a.installation_id IS NOT NULL THEN
            EXECUTE 'SELECT
                (SELECT resource_id FROM olp.provider_resource_pending_usage ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL
                OR
                (SELECT resource_id FROM olp.batch_pending_subjects ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL'
                INTO pending;
        ELSIF a.organization_id IS NOT NULL THEN
            SELECT array_agg(id) INTO projects FROM olp.projects WHERE organization_id = a.organization_id;
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id = ANY($1))
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = ANY($1))'
                INTO pending USING projects;
        ELSIF a.project_id IS NOT NULL AND a.attribution_key IS NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id = $1)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1)'
                INTO pending USING a.project_id;
        ELSIF a.project_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id = $1 AND attribution @> $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1 AND attribution @> $2)'
                INTO pending USING a.project_id, jsonb_build_object(a.attribution_key, a.attribution_value);
        ELSIF a.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_id = $1 AND route_slug = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND route_slug = $2)'
                INTO pending USING a.api_key_id, a.route_slug;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    SELECT * INTO e FROM olp.end_user_accounts WHERE id = owner;
    IF FOUND THEN
        IF e.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.api_key_id, e.end_user_digest;
        ELSE
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_project_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_project_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.project_id, e.end_user_digest;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    RETURN false;
END
$$;
CREATE OR REPLACE FUNCTION olp.has_pending_retained_tokens(owner uuid)
RETURNS boolean LANGUAGE plpgsql STABLE AS $$
DECLARE
    pending boolean;
    a olp.aggregate_budget_accounts;
    e olp.end_user_accounts;
    projects uuid[];
    column_name text;
BEGIN
    SELECT EXISTS(SELECT 1 FROM olp.api_keys k JOIN olp.provider_resource_pending_tokens p ON p.api_key_id=k.id WHERE k.budget_group_id=owner)
     OR EXISTS(SELECT 1 FROM olp.api_keys k JOIN olp.batch_pending_subjects p ON p.api_key_id=k.id WHERE k.budget_group_id=owner) INTO pending;
    IF pending THEN RETURN true; END IF;
    FOREACH column_name IN ARRAY ARRAY[
        'api_key_id', 'budget_group_id', 'provider_id', 'slot_id',
        'route_id', 'pinned_route_id', 'leg_route_id'
    ] LOOP
        EXECUTE format(
            'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE %1$I=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE %1$I=$1)',
            column_name
        ) INTO pending USING owner;
        IF pending THEN RETURN true; END IF;
    END LOOP;
    EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.pending_token_recorded_owners WHERE budget_owner_id=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_recorded_owners WHERE budget_owner_id=$1)'
        INTO pending USING owner;
    IF pending THEN RETURN true; END IF;
    SELECT * INTO a FROM olp.aggregate_budget_accounts WHERE id = owner;
    IF FOUND THEN
        IF a.installation_id IS NOT NULL THEN
            EXECUTE 'SELECT
                (SELECT resource_id FROM olp.provider_resource_pending_tokens ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL
                OR
                (SELECT resource_id FROM olp.batch_pending_subjects ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL'
                INTO pending;
        ELSIF a.organization_id IS NOT NULL THEN
            SELECT array_agg(id) INTO projects FROM olp.projects WHERE organization_id = a.organization_id;
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE project_id = ANY($1))
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = ANY($1))'
                INTO pending USING projects;
        ELSIF a.project_id IS NOT NULL AND a.attribution_key IS NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE project_id = $1)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1)'
                INTO pending USING a.project_id;
        ELSIF a.project_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE project_id = $1 AND attribution @> $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1 AND attribution @> $2)'
                INTO pending USING a.project_id, jsonb_build_object(a.attribution_key, a.attribution_value);
        ELSIF a.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE api_key_id = $1 AND route_slug = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND route_slug = $2)'
                INTO pending USING a.api_key_id, a.route_slug;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    SELECT * INTO e FROM olp.end_user_accounts WHERE id = owner;
    IF FOUND THEN
        IF e.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE api_key_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.api_key_id, e.end_user_digest;
        ELSE
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE api_key_project_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_project_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.project_id, e.end_user_digest;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    RETURN false;
END
$$;

CREATE OR REPLACE FUNCTION olp.price_usage(price jsonb, usage jsonb, started_at timestamptz,
                                successful boolean, discount numeric DEFAULT 1)
RETURNS TABLE(complete boolean, list_cost numeric, cost numeric, components text[])
LANGUAGE plpgsql STABLE AS $$
DECLARE
    rates jsonb := price;
    tier jsonb;
    v_input bigint := (usage->>'input_tokens')::bigint;
    v_output bigint := (usage->>'output_tokens')::bigint;
    v_cached bigint := COALESCE((usage->>'cached_input_tokens')::bigint, 0);
    v_write bigint := COALESCE((usage->>'cache_write_input_tokens')::bigint, 0);
    v_w5m bigint := COALESCE((usage->>'cache_write_5m_input_tokens')::bigint, 0);
    v_w1h bigint := COALESCE((usage->>'cache_write_1h_input_tokens')::bigint, 0);
    v_audio bigint := COALESCE((usage->>'audio_input_tokens')::bigint, 0);
    v_caudio bigint := COALESCE((usage->>'cached_audio_input_tokens')::bigint, 0);
    v_image bigint := COALESCE((usage->>'image_input_tokens')::bigint, 0);
    v_cimage bigint := COALESCE((usage->>'cached_image_input_tokens')::bigint, 0);
    v_aout bigint := COALESCE((usage->>'audio_output_tokens')::bigint, 0);
    v_iout bigint := COALESCE((usage->>'image_output_tokens')::bigint, 0);
    v_media numeric := (usage->>'media_units')::numeric;
    v_batch boolean := COALESCE((usage->>'batch')::boolean, false);
    v_invocations bigint := COALESCE((usage->>'invocation_count')::bigint,CASE WHEN COALESCE((usage->>'deferred')::boolean,false) THEN 0 ELSE 1 END);
    v_ok boolean := price IS NOT NULL;
    v_in_charge numeric := 0;
    v_out_charge numeric := 0;
    v_charge numeric := 0;
    v_multiplier numeric := 1;
    v_comp text[] := '{}';
    share record;
    tool record;
    share_rate numeric;
    share_field text;
    fee numeric;
    local_ts timestamp;
    dow int;
    tod int;
    window_multiplier numeric;
BEGIN
    IF price IS NULL THEN
        RETURN QUERY SELECT false, NULL::numeric, NULL::numeric, NULL::text[];
        RETURN;
    END IF;
    -- Aggregate eval totals cannot choose each invocation's input tier.
    IF COALESCE((usage->>'deferred')::boolean,false) AND v_invocations>1
       AND jsonb_typeof(price->'tiers')='array' THEN
        IF jsonb_array_length(price->'tiers')>0 THEN
            RETURN QUERY SELECT false, NULL::numeric, NULL::numeric, NULL::text[];
            RETURN;
        END IF;
    END IF;

    -- A tier replaces the whole per-million rate set for the attempt whose
    -- input exceeds its threshold; the highest exceeded threshold wins.
    IF v_input IS NOT NULL AND jsonb_typeof(price->'tiers') = 'array' THEN
        SELECT t INTO tier FROM jsonb_array_elements(price->'tiers') AS t
        WHERE (t->>'above_input_tokens')::bigint < v_input
        ORDER BY (t->>'above_input_tokens')::bigint DESC LIMIT 1;
        IF FOUND THEN
            rates := tier;
            v_comp := v_comp || 'tiers'::text;
        END IF;
    END IF;

    -- Input side: every share bills at the first rate its chain carries, and
    -- a nonzero share with no rate leaves the attempt unpriced.
    IF v_input IS NULL THEN
        IF usage->>'cached_input_tokens' IS NOT NULL OR usage->>'cache_write_input_tokens' IS NOT NULL
            OR usage->>'cache_write_5m_input_tokens' IS NOT NULL OR usage->>'cache_write_1h_input_tokens' IS NOT NULL
            OR usage->>'audio_input_tokens' IS NOT NULL OR usage->>'cached_audio_input_tokens' IS NOT NULL
            OR usage->>'image_input_tokens' IS NOT NULL OR usage->>'cached_image_input_tokens' IS NOT NULL THEN
            v_ok := false;
        END IF;
    ELSE
        IF rates->>'input_per_million' IS NULL AND rates->>'audio_input_per_million' IS NULL
            AND rates->>'image_input_per_million' IS NULL THEN
            v_ok := false;
        END IF;
        FOR share IN
            SELECT * FROM (VALUES
                (v_input - v_cached - v_write - (v_audio - v_caudio) - (v_image - v_cimage),
                    ARRAY['input_per_million']),
                (v_cached - v_caudio - v_cimage,
                    ARRAY['cached_input_per_million','input_per_million']),
                (v_caudio,
                    ARRAY['cached_audio_input_per_million','cached_input_per_million','input_per_million']),
                (v_cimage,
                    ARRAY['cached_image_input_per_million','cached_input_per_million','input_per_million']),
                (v_audio - v_caudio,
                    ARRAY['audio_input_per_million','input_per_million']),
                (v_image - v_cimage,
                    ARRAY['image_input_per_million','input_per_million']),
                (v_write - v_w5m - v_w1h,
                    ARRAY['cache_write_input_per_million','input_per_million']),
                (v_w5m,
                    ARRAY['cache_write_5m_input_per_million','cache_write_input_per_million','input_per_million']),
                (v_w1h,
                    ARRAY['cache_write_1h_input_per_million','cache_write_input_per_million','input_per_million'])
            ) AS s(tokens, chain)
        LOOP
            IF share.tokens < 0 THEN
                v_ok := false;
                CONTINUE;
            END IF;
            share_rate := NULL;
            share_field := NULL;
            SELECT r.rate, r.field INTO share_rate, share_field
                FROM olp.price_first_rate(rates, VARIADIC share.chain) r;
            IF share_rate IS NULL THEN
                IF share.tokens > 0 THEN
                    v_ok := false;
                END IF;
            ELSE
                v_in_charge := v_in_charge + share.tokens * share_rate;
                IF share.tokens > 0 AND NOT share_field = ANY(v_comp) THEN
                    v_comp := v_comp || share_field;
                END IF;
            END IF;
        END LOOP;
    END IF;

    -- Output side, with the same share and fallback rules.
    IF v_output IS NULL THEN
        IF usage->>'audio_output_tokens' IS NOT NULL OR usage->>'image_output_tokens' IS NOT NULL THEN
            v_ok := false;
        END IF;
    ELSE
        IF rates->>'output_per_million' IS NULL AND rates->>'audio_output_per_million' IS NULL
            AND rates->>'image_output_per_million' IS NULL THEN
            v_ok := false;
        END IF;
        FOR share IN
            SELECT * FROM (VALUES
                (v_output - v_aout - v_iout, ARRAY['output_per_million']),
                (v_aout, ARRAY['audio_output_per_million','output_per_million']),
                (v_iout, ARRAY['image_output_per_million','output_per_million'])
            ) AS s(tokens, chain)
        LOOP
            IF share.tokens < 0 THEN
                v_ok := false;
                CONTINUE;
            END IF;
            share_rate := NULL;
            share_field := NULL;
            SELECT r.rate, r.field INTO share_rate, share_field
                FROM olp.price_first_rate(rates, VARIADIC share.chain) r;
            IF share_rate IS NULL THEN
                IF share.tokens > 0 THEN
                    v_ok := false;
                END IF;
            ELSE
                v_out_charge := v_out_charge + share.tokens * share_rate;
                IF share.tokens > 0 AND NOT share_field = ANY(v_comp) THEN
                    v_comp := v_comp || share_field;
                END IF;
            END IF;
        END LOOP;
    END IF;

    -- Each side is divided by a million once, the way the rates read.
    v_charge := COALESCE(v_in_charge / 1000000, 0) + COALESCE(v_out_charge / 1000000, 0);

    IF v_media IS NOT NULL THEN
        IF price->>'unit_price' IS NULL THEN
            v_ok := false;
        ELSE
            v_charge := v_charge + v_media * (price->>'unit_price')::numeric;
            IF v_media > 0 THEN
                v_comp := v_comp || 'unit_price'::text;
            END IF;
        END IF;
    END IF;

    IF successful AND v_invocations>0 AND price->>'per_request' IS NOT NULL THEN
        v_charge := v_charge + v_invocations*(price->>'per_request')::numeric;
        v_comp := v_comp || 'per_request'::text;
    END IF;

    IF jsonb_typeof(usage->'tool_calls') = 'object' THEN
        FOR tool IN SELECT key, value::bigint AS calls
            FROM jsonb_each_text(usage->'tool_calls') WHERE value::bigint > 0
        LOOP
            fee := (price->'tool_calls'->>tool.key)::numeric;
            IF fee IS NULL THEN
                v_ok := false;
            ELSE
                v_charge := v_charge + tool.calls * fee;
                v_comp := v_comp || ('tool_calls.' || tool.key);
            END IF;
        END LOOP;
    END IF;

    -- Exact retained units. Omitted quantities remain unknown, never zero.
    FOR share IN SELECT * FROM (VALUES
      ('training_tokens','training_per_million',1000000::numeric,'1000000 Training Tokens'),
      ('storage_byte_seconds','storage_per_gib_day',1073741824::numeric*86400,'GiB Day'),
      ('cache_token_seconds','cache_storage_per_million_token_hour',1000000::numeric*3600,'1000000 Token Hours')
    ) AS dimensions(quantity,rate,divisor,unit) LOOP
      IF usage->>share.quantity IS NOT NULL THEN
        fee:=(price->>share.rate)::numeric;
        IF fee IS NULL THEN v_ok:=false;
        ELSE v_charge:=v_charge+((usage->>share.quantity)::numeric*fee)::numeric(600,300)/share.divisor;
          v_comp:=v_comp||share.rate;
        END IF;
      END IF;
    END LOOP;
    FOR tool IN SELECT key,value::bigint AS calls FROM jsonb_each_text(COALESCE(usage->'container_sessions','{}'::jsonb)) LOOP
      fee:=(price->'container_per_session'->>tool.key)::numeric;
      IF fee IS NULL THEN v_ok:=false;
      ELSE v_charge:=v_charge+tool.calls*fee; v_comp:=v_comp||('container_per_session.'||tool.key);
      END IF;
    END LOOP;

    -- The window multiplier that holds at the attempt's start in the zone the
    -- price names; periods never overlap on a shared day, so at most one does.
    IF jsonb_typeof(price->'windows') = 'object' THEN
        local_ts := started_at AT TIME ZONE (price->'windows'->>'time_zone');
        dow := EXTRACT(ISODOW FROM local_ts)::int;
        tod := EXTRACT(HOUR FROM local_ts)::int * 60 + EXTRACT(MINUTE FROM local_ts)::int;
        window_multiplier := NULL;
        SELECT (p->>'multiplier')::numeric INTO window_multiplier
        FROM jsonb_array_elements(price->'windows'->'periods') AS p
        WHERE EXISTS (SELECT 1 FROM jsonb_array_elements_text(p->'days') d
                      WHERE d::int = dow)
          AND tod >= split_part(p->>'start', ':', 1)::int * 60 + split_part(p->>'start', ':', 2)::int
          AND tod < split_part(p->>'end', ':', 1)::int * 60 + split_part(p->>'end', ':', 2)::int
        LIMIT 1;
        IF FOUND THEN
            v_multiplier := v_multiplier * window_multiplier;
            v_comp := v_comp || 'windows'::text;
        END IF;
    END IF;

    IF v_batch THEN
        IF price->>'batch_multiplier' IS NULL THEN
            v_ok := false;
        ELSE
            v_multiplier := v_multiplier * (price->>'batch_multiplier')::numeric;
            v_comp := v_comp || 'batch_multiplier'::text;
        END IF;
    END IF;

    IF COALESCE(discount, 1) <> 1 THEN
        v_comp := v_comp || 'discount'::text;
    END IF;
    v_ok := v_ok AND COALESCE((usage->>'complete')::boolean, false);
    IF NOT v_ok THEN
        RETURN QUERY SELECT false, NULL::numeric, NULL::numeric, NULL::text[];
        RETURN;
    END IF;
    RETURN QUERY SELECT true,
        round(v_charge * v_multiplier, 12),
        round(v_charge * v_multiplier * COALESCE(discount, 1), 12),
        (SELECT array_agg(c ORDER BY c) FROM (SELECT DISTINCT unnest(v_comp) AS c) sorted);
END $$;

CREATE OR REPLACE FUNCTION olp.price_usage_lines(price jsonb, usage jsonb, started_at timestamptz,
                                successful boolean, discount numeric DEFAULT 1)
RETURNS jsonb
LANGUAGE plpgsql STABLE AS $$
DECLARE
    rates jsonb := price;
    tier jsonb;
    v_tier text;
    v_input bigint := (usage->>'input_tokens')::bigint;
    v_output bigint := (usage->>'output_tokens')::bigint;
    v_cached bigint := COALESCE((usage->>'cached_input_tokens')::bigint, 0);
    v_write bigint := COALESCE((usage->>'cache_write_input_tokens')::bigint, 0);
    v_w5m bigint := COALESCE((usage->>'cache_write_5m_input_tokens')::bigint, 0);
    v_w1h bigint := COALESCE((usage->>'cache_write_1h_input_tokens')::bigint, 0);
    v_audio bigint := COALESCE((usage->>'audio_input_tokens')::bigint, 0);
    v_caudio bigint := COALESCE((usage->>'cached_audio_input_tokens')::bigint, 0);
    v_image bigint := COALESCE((usage->>'image_input_tokens')::bigint, 0);
    v_cimage bigint := COALESCE((usage->>'cached_image_input_tokens')::bigint, 0);
    v_aout bigint := COALESCE((usage->>'audio_output_tokens')::bigint, 0);
    v_iout bigint := COALESCE((usage->>'image_output_tokens')::bigint, 0);
    v_media numeric := (usage->>'media_units')::numeric;
    v_batch boolean := COALESCE((usage->>'batch')::boolean, false);
    v_invocations bigint := COALESCE((usage->>'invocation_count')::bigint,CASE WHEN COALESCE((usage->>'deferred')::boolean,false) THEN 0 ELSE 1 END);
    v_ok boolean := price IS NOT NULL;
    v_multiplier numeric := 1;
    v_base jsonb := '[]'::jsonb;
    v_result jsonb;
    share record;
    tool record;
    share_rate numeric;
    share_field text;
    fee numeric;
    local_ts timestamp;
    dow int;
    tod int;
    window_multiplier numeric;
BEGIN
    IF price IS NULL THEN
        RETURN NULL;
    END IF;
    -- Aggregate eval totals cannot choose each invocation's input tier.
    IF COALESCE((usage->>'deferred')::boolean,false) AND v_invocations>1
       AND jsonb_typeof(price->'tiers')='array' THEN
        IF jsonb_array_length(price->'tiers')>0 THEN
            RETURN NULL;
        END IF;
    END IF;

    -- A tier replaces the whole per-million rate set for the attempt whose
    -- input exceeds its threshold; the highest exceeded threshold wins.
    IF v_input IS NOT NULL AND jsonb_typeof(price->'tiers') = 'array' THEN
        SELECT t INTO tier FROM jsonb_array_elements(price->'tiers') AS t
        WHERE (t->>'above_input_tokens')::bigint < v_input
        ORDER BY (t->>'above_input_tokens')::bigint DESC LIMIT 1;
        IF FOUND THEN
            rates := tier;
            v_tier := tier->>'above_input_tokens';
        END IF;
    END IF;

    -- Input side: every share bills at the first rate its chain carries, and
    -- a nonzero share with no rate leaves the attempt unpriced.
    IF v_input IS NULL THEN
        IF usage->>'cached_input_tokens' IS NOT NULL OR usage->>'cache_write_input_tokens' IS NOT NULL
            OR usage->>'cache_write_5m_input_tokens' IS NOT NULL OR usage->>'cache_write_1h_input_tokens' IS NOT NULL
            OR usage->>'audio_input_tokens' IS NOT NULL OR usage->>'cached_audio_input_tokens' IS NOT NULL
            OR usage->>'image_input_tokens' IS NOT NULL OR usage->>'cached_image_input_tokens' IS NOT NULL THEN
            v_ok := false;
        END IF;
    ELSE
        IF rates->>'input_per_million' IS NULL AND rates->>'audio_input_per_million' IS NULL
            AND rates->>'image_input_per_million' IS NULL THEN
            v_ok := false;
        END IF;
        FOR share IN
            SELECT * FROM (VALUES
                (v_input - v_cached - v_write - (v_audio - v_caudio) - (v_image - v_cimage),
                    ARRAY['input_per_million']),
                (v_cached - v_caudio - v_cimage,
                    ARRAY['cached_input_per_million','input_per_million']),
                (v_caudio,
                    ARRAY['cached_audio_input_per_million','cached_input_per_million','input_per_million']),
                (v_cimage,
                    ARRAY['cached_image_input_per_million','cached_input_per_million','input_per_million']),
                (v_audio - v_caudio,
                    ARRAY['audio_input_per_million','input_per_million']),
                (v_image - v_cimage,
                    ARRAY['image_input_per_million','input_per_million']),
                (v_write - v_w5m - v_w1h,
                    ARRAY['cache_write_input_per_million','input_per_million']),
                (v_w5m,
                    ARRAY['cache_write_5m_input_per_million','cache_write_input_per_million','input_per_million']),
                (v_w1h,
                    ARRAY['cache_write_1h_input_per_million','cache_write_input_per_million','input_per_million'])
            ) AS s(tokens, chain)
        LOOP
            IF share.tokens < 0 THEN
                v_ok := false;
                CONTINUE;
            END IF;
            share_rate := NULL;
            share_field := NULL;
            SELECT r.rate, r.field INTO share_rate, share_field
                FROM olp.price_first_rate(rates, VARIADIC share.chain) r;
            IF share_rate IS NULL THEN
                IF share.tokens > 0 THEN
                    v_ok := false;
                END IF;
            ELSIF share.tokens > 0 THEN
                v_base := v_base || jsonb_build_object('component', share_field, 'tier', v_tier,
                    'unit', '1000000 Tokens', 'quantity', (share.tokens::numeric / 1000000)::text,
                    'base', share_rate::text);
            END IF;
        END LOOP;
    END IF;

    -- Output side, with the same share and fallback rules.
    IF v_output IS NULL THEN
        IF usage->>'audio_output_tokens' IS NOT NULL OR usage->>'image_output_tokens' IS NOT NULL THEN
            v_ok := false;
        END IF;
    ELSE
        IF rates->>'output_per_million' IS NULL AND rates->>'audio_output_per_million' IS NULL
            AND rates->>'image_output_per_million' IS NULL THEN
            v_ok := false;
        END IF;
        FOR share IN
            SELECT * FROM (VALUES
                (v_output - v_aout - v_iout, ARRAY['output_per_million']),
                (v_aout, ARRAY['audio_output_per_million','output_per_million']),
                (v_iout, ARRAY['image_output_per_million','output_per_million'])
            ) AS s(tokens, chain)
        LOOP
            IF share.tokens < 0 THEN
                v_ok := false;
                CONTINUE;
            END IF;
            share_rate := NULL;
            share_field := NULL;
            SELECT r.rate, r.field INTO share_rate, share_field
                FROM olp.price_first_rate(rates, VARIADIC share.chain) r;
            IF share_rate IS NULL THEN
                IF share.tokens > 0 THEN
                    v_ok := false;
                END IF;
            ELSIF share.tokens > 0 THEN
                v_base := v_base || jsonb_build_object('component', share_field, 'tier', v_tier,
                    'unit', '1000000 Tokens', 'quantity', (share.tokens::numeric / 1000000)::text,
                    'base', share_rate::text);
            END IF;
        END LOOP;
    END IF;

    -- Media, per-request and tool lines bill from the entry itself: a tier
    -- never rewrites them, so their tier member stays null.
    IF v_media IS NOT NULL THEN
        IF price->>'unit_price' IS NULL THEN
            v_ok := false;
        ELSE
            IF v_media > 0 THEN
                v_base := v_base || jsonb_build_object('component', 'unit_price', 'tier', NULL,
                    'unit', 'Units', 'quantity', v_media::text,
                    'base', (price->>'unit_price')::numeric::text);
            END IF;
        END IF;
    END IF;

    IF successful AND v_invocations>0 AND price->>'per_request' IS NOT NULL THEN
        v_base := v_base || jsonb_build_object('component', 'per_request', 'tier', NULL,
            'unit', 'Requests', 'quantity', v_invocations::text,
            'base', (price->>'per_request')::numeric::text);
    END IF;

    IF jsonb_typeof(usage->'tool_calls') = 'object' THEN
        FOR tool IN SELECT key, value::bigint AS calls
            FROM jsonb_each_text(usage->'tool_calls') WHERE value::bigint > 0
        LOOP
            fee := (price->'tool_calls'->>tool.key)::numeric;
            IF fee IS NULL THEN
                v_ok := false;
            ELSE
                v_base := v_base || jsonb_build_object('component', 'tool_calls.' || tool.key, 'tier', NULL,
                    'unit', 'Calls', 'quantity', tool.calls::text,
                    'base', fee::text);
            END IF;
        END LOOP;
    END IF;

    -- Exact retained units. Omitted quantities remain unknown, never zero.
    FOR share IN SELECT * FROM (VALUES
      ('training_tokens','training_per_million',1000000::numeric,'1000000 Training Tokens'),
      ('storage_byte_seconds','storage_per_gib_day',1073741824::numeric*86400,'GiB Day'),
      ('cache_token_seconds','cache_storage_per_million_token_hour',1000000::numeric*3600,'1000000 Token Hours')
    ) AS dimensions(quantity,rate,divisor,unit) LOOP
      IF usage->>share.quantity IS NOT NULL THEN
        fee:=(price->>share.rate)::numeric;
        IF fee IS NULL THEN v_ok:=false;
        ELSIF (usage->>share.quantity)::numeric>0 THEN
          v_base:=v_base||jsonb_build_object('component',share.rate,'tier',NULL,'unit',share.unit,
            'quantity',((usage->>share.quantity)::numeric(600,300)/share.divisor)::text,'base',fee::text);
        END IF;
      END IF;
    END LOOP;
    FOR tool IN SELECT key,value::bigint AS calls FROM jsonb_each_text(COALESCE(usage->'container_sessions','{}'::jsonb)) LOOP
      fee:=(price->'container_per_session'->>tool.key)::numeric;
      IF fee IS NULL THEN v_ok:=false;
      ELSIF tool.calls>0 THEN v_base:=v_base||jsonb_build_object('component','container_per_session.'||tool.key,'tier',tool.key,
        'unit','Session','quantity',tool.calls::text,'base',fee::text);
      END IF;
    END LOOP;

    -- The window multiplier that holds at the attempt's start in the zone the
    -- price names; periods never overlap on a shared day, so at most one does.
    IF jsonb_typeof(price->'windows') = 'object' THEN
        local_ts := started_at AT TIME ZONE (price->'windows'->>'time_zone');
        dow := EXTRACT(ISODOW FROM local_ts)::int;
        tod := EXTRACT(HOUR FROM local_ts)::int * 60 + EXTRACT(MINUTE FROM local_ts)::int;
        window_multiplier := NULL;
        SELECT (p->>'multiplier')::numeric INTO window_multiplier
        FROM jsonb_array_elements(price->'windows'->'periods') AS p
        WHERE EXISTS (SELECT 1 FROM jsonb_array_elements_text(p->'days') d
                      WHERE d::int = dow)
          AND tod >= split_part(p->>'start', ':', 1)::int * 60 + split_part(p->>'start', ':', 2)::int
          AND tod < split_part(p->>'end', ':', 1)::int * 60 + split_part(p->>'end', ':', 2)::int
        LIMIT 1;
        IF FOUND THEN
            v_multiplier := v_multiplier * window_multiplier;
        END IF;
    END IF;

    IF v_batch THEN
        IF price->>'batch_multiplier' IS NULL THEN
            v_ok := false;
        ELSE
            v_multiplier := v_multiplier * (price->>'batch_multiplier')::numeric;
        END IF;
    END IF;

    v_ok := v_ok AND COALESCE((usage->>'complete')::boolean, false);
    IF NOT v_ok THEN
        RETURN NULL;
    END IF;

    -- Rates become unit prices under the window and batch multipliers and the
    -- consumer discount; equal lines merge on summed quantity, and the result
    -- orders deterministically so equal inputs marshal to identical bytes.
    SELECT jsonb_agg(jsonb_build_object(
               'component', g.component, 'tier', g.tier, 'unit', g.unit,
               'quantity', trim_scale(g.quantity)::text,
               'list_unit_price', g.list_price::text,
               'contracted_unit_price', g.contracted_price::text)
           ORDER BY g.component, g.tier, g.list_price)
    INTO v_result
    FROM (
        SELECT x.component, x.tier, x.unit, sum(x.quantity) AS quantity,
               x.list_price, x.contracted_price
        FROM (
            SELECT l->>'component' AS component, l->>'tier' AS tier, l->>'unit' AS unit,
                   (l->>'quantity')::numeric AS quantity,
                   trim_scale((l->>'base')::numeric * v_multiplier) AS list_price,
                   trim_scale((l->>'base')::numeric * v_multiplier * COALESCE(discount, 1)) AS contracted_price
            FROM jsonb_array_elements(v_base) l
        ) x
        GROUP BY x.component, x.tier, x.unit, x.list_price, x.contracted_price
    ) g;
    RETURN COALESCE(v_result, '[]'::jsonb);
END $$;

CREATE FUNCTION olp.container_session_totals(documents jsonb) RETURNS jsonb LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT jsonb_object_agg(tier,quantity) FROM (
 SELECT e.key AS tier,sum(e.value::numeric) AS quantity FROM jsonb_array_elements(documents) d
 CROSS JOIN LATERAL jsonb_each_text(CASE WHEN jsonb_typeof(d)='object' THEN d ELSE '{}'::jsonb END) e GROUP BY e.key) counts;
$$;

CREATE FUNCTION olp.container_session_string_totals(documents jsonb) RETURNS jsonb LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT jsonb_object_agg(key,value) FROM jsonb_each_text(olp.container_session_totals(documents));
$$;

CREATE OR REPLACE FUNCTION olp.export_accounted_request() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_request olp.requests%ROWTYPE; v_project uuid; v_outcome text; v_attempt record; v_rollup record; v_meter record;
BEGIN
 IF NEW.status<>'fact_persisted' OR OLD.status='fact_persisted' THEN RETURN NEW; END IF;
 IF NOT EXISTS(SELECT 1 FROM olp.export_sinks WHERE enabled AND streams && ARRAY['requests','attempts','usage_rollups','guardrail_decisions','metering','focus']::text[]) THEN RETURN NEW; END IF;
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
 FOR v_meter IN
  SELECT f.attempt_id,f.route_slug,f.observed_at,
   COALESCE(k.project_id,pr.project_id,rt.project_id) AS consumer,
   CASE WHEN a.id IS NULL OR (a.error_class IS NULL AND a.status_code BETWEEN 200 AND 299)
        THEN 'success' ELSE 'failure' END AS attempt_outcome,
   jsonb_build_object(
    'attempt_id',f.attempt_id,'request_id',f.request_id,'observed_at',f.observed_at,
    'project_id',COALESCE(k.project_id,pr.project_id,rt.project_id),
    'api_key_id',f.api_key_id,'route',f.route_slug,'provider_id',f.provider_id,
    'upstream_model',f.upstream_model,'operation',f.operation,'surface',f.surface,
    'input_tokens',f.input_tokens,'output_tokens',f.output_tokens,
    'cached_input_tokens',f.cached_input_tokens,'cache_write_input_tokens',f.cache_write_input_tokens,
    'cache_write_5m_input_tokens',f.cache_write_5m_input_tokens,'cache_write_1h_input_tokens',f.cache_write_1h_input_tokens,
    'audio_input_tokens',f.audio_input_tokens,'cached_audio_input_tokens',f.cached_audio_input_tokens,
    'audio_output_tokens',f.audio_output_tokens,
    'image_input_tokens',f.image_input_tokens,'cached_image_input_tokens',f.cached_image_input_tokens,
    'image_output_tokens',f.image_output_tokens,
    'invocation_count',f.invocation_count,'training_tokens',f.training_tokens,'storage_byte_seconds',f.storage_byte_seconds::text,'cache_token_seconds',f.cache_token_seconds::text,'container_sessions',f.container_sessions,
    'media_units',f.media_units::text,'tool_calls',f.tool_calls,'batch',f.batch,
    'cost',f.estimated_cost::text,'charge',f.charge::text,'list_cost',f.list_cost::text,
    'currency',btrim(f.currency),'pricing_revision_id',f.pricing_revision_id,
    'rate_card_revision_id',f.rate_card_revision_id,'discount_id',f.discount_id,
    'commitment_weight',f.commitment_weight::text,
    'attribution',f.attribution,'end_user_digest',f.end_user_digest) AS payload
  FROM olp.attempt_usage_facts f
  LEFT JOIN olp.attempts a ON a.id=f.attempt_id
  LEFT JOIN olp.api_keys k ON k.id=f.api_key_id
  LEFT JOIN olp.providers pr ON pr.id=f.provider_id
  LEFT JOIN olp.routes rt ON rt.slug=f.route_slug
  WHERE f.event_id=NEW.event_id AND f.charge_status='billable' AND f.usage_complete
   AND (f.estimated_cost IS NOT NULL OR f.charge IS NOT NULL)
 LOOP
  PERFORM olp.enqueue_export('metering',v_meter.attempt_id,v_meter.consumer,v_meter.route_slug,
   v_meter.attempt_outcome,v_meter.observed_at,v_meter.payload);
 END LOOP;
 -- A focus dataset restates a whole day, so a priced fact dirties its UTC
 -- day. A committed fact also dirties every day of the commitment bucket
 -- that might already hold a delivered revision.
 IF EXISTS(SELECT 1 FROM olp.export_sinks WHERE enabled AND 'focus' = ANY(streams)) THEN
  INSERT INTO olp.focus_dirty_days(day,version)
  SELECT DISTINCT (f.observed_at AT TIME ZONE 'UTC')::date,nextval('olp.focus_dirty_version')
  FROM olp.attempt_usage_facts f WHERE f.event_id=NEW.event_id
  UNION
  SELECT d.day,nextval('olp.focus_dirty_version')
  FROM olp.attempt_usage_facts f
  CROSS JOIN LATERAL (
   SELECT generate_series(
    date_trunc(f.commitment_period,f.observed_at AT TIME ZONE 'UTC')::date,
    least((date_trunc(f.commitment_period,f.observed_at AT TIME ZONE 'UTC')
           + ('1 '||f.commitment_period)::interval)::date - 1,
          (now() AT TIME ZONE 'UTC')::date),
    '1 day'::interval)::date AS day
  ) d WHERE f.event_id=NEW.event_id AND f.commitment_period IS NOT NULL
  ON CONFLICT (day) DO UPDATE SET version=nextval('olp.focus_dirty_version');
 END IF;
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
   jsonb_build_object('training_tokens',sum(f.training_tokens),'storage_byte_seconds',sum(f.storage_byte_seconds)::text,'cache_token_seconds',sum(f.cache_token_seconds)::text,'container_sessions',olp.container_session_totals(jsonb_agg(f.container_sessions))) ||
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

CREATE INDEX retained_keys_current_group ON olp.api_keys(budget_group_id,id) WHERE budget_group_id IS NOT NULL;

ALTER TABLE olp.attempt_usage_hourly ADD COLUMN training_tokens numeric NOT NULL DEFAULT 0 CHECK(training_tokens>=0),
 ADD COLUMN storage_byte_seconds numeric NOT NULL DEFAULT 0 CHECK(storage_byte_seconds>=0),
 ADD COLUMN cache_token_seconds numeric NOT NULL DEFAULT 0 CHECK(cache_token_seconds>=0),
 ADD COLUMN container_sessions jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE olp.worker_task_health DROP CONSTRAINT worker_task_health_task_check,
 ADD CONSTRAINT worker_task_health_task_check CHECK(task IN ('request_metadata_consumer','maintenance','cost_reconciliation','request_metadata_gateway_epoch_detection','media_reconciliation','notification_delivery','grant_refresh','health_probes','export_delivery','managed_export_delivery','focus_export','billing_import','batch_settlement','execution_debits','retained_settlement'));

CREATE FUNCTION olp.guard_retained_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.resource_id,NEW.operation,NEW.templates,NEW.token_consuming,NEW.started_at)
   IS DISTINCT FROM (OLD.resource_id,OLD.operation,OLD.templates,OLD.token_consuming,OLD.started_at)
   OR OLD.state='closed' AND NEW.state<>'closed'
   OR OLD.remote_terminated AND NOT NEW.remote_terminated
   OR OLD.terminal_at IS NOT NULL AND NEW.terminal_at IS DISTINCT FROM OLD.terminal_at THEN
  RAISE EXCEPTION 'retained acceptance and settlement evidence is immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retained_settlement_pins BEFORE UPDATE ON olp.retained_settlements FOR EACH ROW EXECUTE FUNCTION olp.guard_retained_settlement();
CREATE TRIGGER retained_evidence_immutable BEFORE UPDATE OR DELETE ON olp.retained_usage_evidence FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
CREATE TRIGGER retained_coverage_immutable BEFORE UPDATE OR DELETE ON olp.retained_usage_coverage FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
CREATE FUNCTION olp.guard_retained_resource_pin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.contract_version='retained-resources-1' AND
 (NEW.kind,NEW.api_key_id,NEW.route_slug,NEW.provider_id,NEW.provider_revision_id,NEW.route_revision_id,NEW.target_id,NEW.slot_id,NEW.credential_id,NEW.parent_id,NEW.contract_version)
 IS DISTINCT FROM
 (OLD.kind,OLD.api_key_id,OLD.route_slug,OLD.provider_id,OLD.provider_revision_id,OLD.route_revision_id,OLD.target_id,OLD.slot_id,OLD.credential_id,OLD.parent_id,OLD.contract_version) THEN
  RAISE EXCEPTION 'retained resource authority is immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retained_resource_pins BEFORE UPDATE ON olp.provider_resources FOR EACH ROW EXECUTE FUNCTION olp.guard_retained_resource_pin();

ALTER TABLE olp.requests DROP CONSTRAINT requests_origin_check,DROP CONSTRAINT requests_origin_parent_check,
 ADD CONSTRAINT requests_origin_check CHECK(origin IN ('caller','shadow','classifier','probe','playground','cache_embedding','batch_settlement','retained_settlement')),
 ADD CONSTRAINT requests_origin_parent_check CHECK((parent_request_id IS NOT NULL)=(origin IN ('shadow','classifier','cache_embedding','batch_settlement','retained_settlement') OR evaluator_guardrail_id IS NOT NULL));

ALTER TABLE olp.prices ADD CONSTRAINT retained_container_rates_object CHECK(container_per_session IS NULL OR jsonb_typeof(container_per_session)='object');
ALTER TABLE olp.rate_card_prices ADD CONSTRAINT retained_card_container_rates_object CHECK(container_per_session IS NULL OR jsonb_typeof(container_per_session)='object');
ALTER TABLE olp.attempt_usage_facts ADD CONSTRAINT retained_container_sessions_object CHECK(container_sessions IS NULL OR jsonb_typeof(container_sessions)='object');
