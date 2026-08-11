-- Preserve complete, queryable usage dimensions after raw facts age into the
-- retained hourly store. Pricing is installation-wide, so every priced fact
-- and aggregate carries the one configured currency rather than silently
-- combining unrelated monetary units.

CREATE TABLE pricing_currency (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    currency char(3) NOT NULL
        CHECK (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$')
);

ALTER TABLE prices ADD CONSTRAINT prices_currency_format_check
    CHECK (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$');

DO $$
DECLARE
    currency_count bigint;
    existing_currency char(3);
BEGIN
    SELECT count(DISTINCT upper(btrim(currency))), min(upper(btrim(currency)))
      INTO currency_count, existing_currency
      FROM prices;
    IF currency_count > 1 THEN
        RAISE EXCEPTION 'existing pricing revisions contain mixed currencies';
    END IF;
    IF currency_count = 1 THEN
        INSERT INTO pricing_currency (singleton, currency)
        VALUES (true, existing_currency);
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_installation_pricing_currency() RETURNS trigger
LANGUAGE plpgsql AS $$
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

CREATE TRIGGER prices_installation_currency_guard
BEFORE INSERT OR UPDATE OF currency ON prices
FOR EACH ROW EXECUTE FUNCTION enforce_installation_pricing_currency();

ALTER TABLE usage_facts
    -- Pre-0010 facts did not carry their originating protocol. Start from an
    -- honest sentinel and recover the exact value from retained request
    -- metadata below; never silently attribute legacy usage to OpenAI.
    ADD COLUMN surface text NOT NULL DEFAULT 'unknown',
    ADD COLUMN currency char(3),
    ADD CONSTRAINT usage_facts_surface_check
        CHECK (surface IN ('open_ai', 'anthropic', 'gemini', 'unknown')),
    ADD CONSTRAINT usage_facts_currency_check
        CHECK (currency IS NULL OR
               (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$'));

UPDATE usage_facts fact
   SET surface = CASE request.surface
        WHEN 'openai' THEN 'open_ai'
        ELSE request.surface
       END
  FROM requests request
 WHERE request.id = fact.request_id
   AND request.started_at = fact.request_started_at
   AND request.surface IN ('open_ai', 'openai', 'anthropic', 'gemini');

-- Request metadata is retained for less time than raw usage facts. An upgrade
-- can therefore encounter facts whose source surface can no longer be
-- reconstructed. Keep those facts queryable under `unknown`, make their
-- incompleteness explicit, and publish a durable gap covering the affected
-- interval so cost-completeness APIs cannot report a false clean bill of
-- health.
DO $$
DECLARE
    unknown_count bigint;
    first_unknown timestamptz;
    last_unknown timestamptz;
BEGIN
    SELECT count(*), min(observed_at), max(observed_at)
      INTO unknown_count, first_unknown, last_unknown
      FROM usage_facts
     WHERE surface = 'unknown';

    IF unknown_count > 0 THEN
        UPDATE usage_facts
           SET usage_complete = false
         WHERE surface = 'unknown';

        INSERT INTO usage_ingestion_gaps
            (id, gateway_instance, event_count, reason,
             first_observed_at, last_observed_at)
        VALUES
            (gen_random_uuid(), 'migration-0010', unknown_count,
             'pre_0010_usage_surface_unknown', first_unknown, last_unknown);
    END IF;
END;
$$;

-- New writers must always provide the canonical surface explicitly.
ALTER TABLE usage_facts ALTER COLUMN surface DROP DEFAULT;

UPDATE usage_facts fact
   SET currency = configured.currency
  FROM pricing_currency configured
 WHERE configured.singleton
   AND fact.pricing_revision_id IS NOT NULL;

ALTER TABLE usage_hourly DROP CONSTRAINT usage_hourly_pkey;
ALTER TABLE usage_hourly
    -- Retained aggregates have no request identity to join through. Aggregates
    -- rebuilt from raw facts are removed below and regain an exact surface on
    -- the next maintenance pass; genuinely cold aggregates remain unknown.
    ADD COLUMN surface text NOT NULL DEFAULT 'unknown',
    ADD COLUMN api_key_id uuid REFERENCES api_keys(id),
    ADD COLUMN cached_input_tokens numeric(30, 0) NOT NULL DEFAULT 0,
    ADD COLUMN media_units numeric(30, 6) NOT NULL DEFAULT 0,
    ADD COLUMN currency char(3),
    ADD CONSTRAINT usage_hourly_surface_check
        CHECK (surface IN ('open_ai', 'anthropic', 'gemini', 'unknown')),
    ADD CONSTRAINT usage_hourly_cached_input_check CHECK (cached_input_tokens >= 0),
    ADD CONSTRAINT usage_hourly_media_units_check CHECK (media_units >= 0),
    ADD CONSTRAINT usage_hourly_currency_check
        CHECK (currency IS NULL OR
               (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$')),
    ADD CONSTRAINT usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
        (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id);

UPDATE usage_hourly hourly
   SET currency = configured.currency
  FROM pricing_currency configured
 WHERE configured.singleton
   AND hourly.estimated_cost IS NOT NULL;

-- Old maintenance versions materialized aggregates while retaining the same
-- raw facts. Remove only overlapping aggregate buckets so the new UNION ALL
-- query path cannot double count after a rolling upgrade. Cold aggregates for
-- facts already removed by retention remain available (with an unknown key).
DELETE FROM usage_hourly hourly
 WHERE EXISTS (
    SELECT 1
      FROM usage_facts fact
     WHERE date_trunc('hour', fact.observed_at) = hourly.bucket
 );

-- Any aggregates left after removing raw-overlapping buckets cannot be rebuilt
-- with an exact surface. Preserve their totals without inventing an OpenAI
-- attribution and make the retained interval visibly incomplete.
DO $$
DECLARE
    unknown_count bigint;
    first_unknown timestamptz;
    last_unknown timestamptz;
BEGIN
    SELECT COALESCE(sum(request_count), 0), min(bucket),
           max(bucket + interval '1 hour')
      INTO unknown_count, first_unknown, last_unknown
      FROM usage_hourly
     WHERE surface = 'unknown';

    IF unknown_count > 0 THEN
        UPDATE usage_hourly
           SET incomplete_count = request_count
         WHERE surface = 'unknown';

        INSERT INTO usage_ingestion_gaps
            (id, gateway_instance, event_count, reason,
             first_observed_at, last_observed_at)
        VALUES
            (gen_random_uuid(), 'migration-0010', unknown_count,
             'pre_0010_hourly_surface_unknown', first_unknown, last_unknown);
    END IF;
END;
$$;

-- Maintenance writes an explicit surface when it builds every new aggregate.
ALTER TABLE usage_hourly ALTER COLUMN surface DROP DEFAULT;

CREATE TABLE usage_loss_reporter_state (
    gateway_instance text PRIMARY KEY,
    process_epoch uuid NOT NULL,
    dropped bigint NOT NULL CHECK (dropped >= 0),
    abandoned bigint NOT NULL CHECK (abandoned >= 0),
    updated_at timestamptz NOT NULL
);

CREATE INDEX transactional_outbox_published_idx
    ON transactional_outbox(published_at)
    WHERE published_at IS NOT NULL;
