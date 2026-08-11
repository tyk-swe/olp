ALTER TABLE installation RENAME COLUMN organization_name TO installation_name;

-- The delivery envelope carries request/attempt metadata and may include real
-- usage quantities, but it is not itself the usage accounting surface. Rename
-- the schema objects in place so every persisted row and foreign-key identity
-- is preserved.
ALTER TYPE usage_event_receipt_status RENAME TO request_metadata_event_receipt_status;
ALTER TYPE usage_gap_certainty RENAME TO request_metadata_gap_certainty;

ALTER TABLE usage_event_receipts RENAME TO request_metadata_event_receipts;
ALTER TABLE usage_consumer_health RENAME TO request_metadata_consumer_health;
ALTER TABLE usage_gateway_epochs RENAME TO request_metadata_gateway_epochs;
ALTER TABLE usage_ingestion_gaps RENAME TO request_metadata_ingestion_gaps;
ALTER TABLE usage_gap_hourly RENAME TO request_metadata_gap_hourly;
ALTER TABLE usage_loss_reporter_state RENAME TO request_metadata_loss_reporter_state;

ALTER INDEX usage_event_receipts_recorded_at_idx
    RENAME TO request_metadata_event_receipts_recorded_at_idx;
ALTER INDEX usage_gateway_epochs_one_open_idx
    RENAME TO request_metadata_gateway_epochs_one_open_idx;
ALTER INDEX usage_gateway_epochs_process_epoch_idx
    RENAME TO request_metadata_gateway_epochs_process_epoch_idx;
ALTER INDEX usage_gateway_epochs_stale_scan_idx
    RENAME TO request_metadata_gateway_epochs_stale_scan_idx;
ALTER INDEX usage_gateway_epochs_unresolved_idx
    RENAME TO request_metadata_gateway_epochs_unresolved_idx;
ALTER INDEX usage_ingestion_gaps_deduplication_key_idx
    RENAME TO request_metadata_ingestion_gaps_deduplication_key_idx;
ALTER INDEX usage_gap_hourly_overlap_idx
    RENAME TO request_metadata_gap_hourly_overlap_idx;

-- PostgreSQL keeps constraint names when a table is renamed. Rename every
-- generated and explicit usage-prefixed constraint without assuming the set of
-- generated CHECK suffixes from a particular PostgreSQL release.
DO $$
DECLARE
    relation_name text;
    constraint_name text;
BEGIN
    FOREACH relation_name IN ARRAY ARRAY[
        'request_metadata_event_receipts',
        'request_metadata_consumer_health',
        'request_metadata_gateway_epochs',
        'request_metadata_ingestion_gaps',
        'request_metadata_gap_hourly',
        'request_metadata_loss_reporter_state'
    ]
    LOOP
        FOR constraint_name IN
            SELECT conname
              FROM pg_constraint
             WHERE conrelid = relation_name::regclass
               AND conname LIKE 'usage\_%' ESCAPE '\'
        LOOP
            EXECUTE format(
                'ALTER TABLE %I RENAME CONSTRAINT %I TO %I',
                relation_name,
                constraint_name,
                regexp_replace(constraint_name, '^usage_', 'request_metadata_')
            );
        END LOOP;
    END LOOP;
END;
$$;

ALTER FUNCTION enforce_usage_fact_receipt()
    RENAME TO enforce_request_metadata_fact_receipt;
ALTER FUNCTION preserve_usage_fact_receipt()
    RENAME TO preserve_request_metadata_fact_receipt;
ALTER TRIGGER usage_facts_receipt_guard ON usage_facts
    RENAME TO usage_facts_request_metadata_receipt_guard;
ALTER TRIGGER usage_facts_preserve_receipt ON usage_facts
    RENAME TO usage_facts_preserve_request_metadata_receipt;
ALTER TRIGGER usage_gap_hourly_writer_guard ON request_metadata_gap_hourly
    RENAME TO request_metadata_gap_hourly_writer_guard;

CREATE OR REPLACE FUNCTION enforce_request_metadata_fact_receipt() RETURNS trigger AS $$
BEGIN
    UPDATE request_metadata_event_receipts
    SET status = 'fact_persisted'
    WHERE event_id = NEW.id
      AND request_id = NEW.request_id
      AND status = 'pending';
    IF FOUND THEN
        RETURN NEW;
    END IF;

    IF EXISTS (
        SELECT 1 FROM request_metadata_event_receipts
        WHERE event_id = NEW.id
          AND request_id = NEW.request_id
          AND status IN ('fact_persisted', 'rejected')
    ) THEN
        RETURN NULL;
    END IF;

    IF EXISTS (
        SELECT 1 FROM usage_facts
        WHERE id = NEW.id AND request_id = NEW.request_id
    ) THEN
        RETURN NULL;
    END IF;

    IF EXISTS (
        SELECT 1 FROM usage_facts
        WHERE id = NEW.id OR request_id = NEW.request_id
    ) THEN
        RAISE EXCEPTION 'usage fact identity conflicts with an existing fact'
            USING ERRCODE = '55000';
    END IF;

    IF NEW.observed_at < now() - interval '7 days'
       OR NEW.observed_at > now() + interval '5 minutes' THEN
        INSERT INTO request_metadata_event_receipts
            (event_id, request_id, event_sha256, status, observed_at)
        VALUES (NEW.id, NEW.request_id, NULL, 'rejected', NEW.observed_at)
        ON CONFLICT DO NOTHING;
        IF FOUND THEN
            INSERT INTO request_metadata_ingestion_gaps
                (id, gateway_instance, event_count, reason, certainty,
                 first_observed_at, last_observed_at)
            VALUES
                (gen_random_uuid(), 'database-fence', 0,
                 'request_metadata_event_outside_replay_window',
                 'lower_bound'::request_metadata_gap_certainty, now(), now());
        END IF;
        RETURN NULL;
    END IF;

    INSERT INTO request_metadata_event_receipts
        (event_id, request_id, event_sha256, status, observed_at)
    VALUES (NEW.id, NEW.request_id, NULL, 'fact_persisted', NEW.observed_at);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION preserve_request_metadata_fact_receipt() RETURNS trigger AS $$
BEGIN
    IF OLD.observed_at >= now() - interval '7 days' THEN
        INSERT INTO request_metadata_event_receipts
            (event_id, request_id, event_sha256, status, observed_at)
        VALUES (OLD.id, OLD.request_id, NULL, 'fact_persisted', OLD.observed_at)
        ON CONFLICT DO NOTHING;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

-- Migration 0023 guards both additive rollup tables at statement level. Enable
-- its transaction-local writer fence before the first guarded UPDATE.
SET LOCAL olp.usage_rollup_writer = 'additive-v2';

UPDATE request_metadata_ingestion_gaps
   SET reason = 'request_metadata_event_outside_replay_window'
 WHERE reason = 'usage_event_outside_replay_window';

UPDATE request_metadata_ingestion_gaps
   SET reason = regexp_replace(
           reason,
           '^fresh_valkey_stream_loss_',
           'request_metadata_stream_loss_'
       )
 WHERE reason LIKE 'fresh\_valkey\_stream\_loss\_%' ESCAPE '\';

UPDATE request_metadata_ingestion_gaps
   SET deduplication_key = regexp_replace(
           deduplication_key,
           '^fresh_valkey_stream_loss_',
           'request_metadata_stream_loss_'
       )
 WHERE deduplication_key LIKE 'fresh\_valkey\_stream\_loss\_%' ESCAPE '\';

UPDATE request_metadata_gap_hourly
   SET reason = regexp_replace(
           reason,
           '^fresh_valkey_stream_loss_',
           'request_metadata_stream_loss_'
       )
 WHERE reason LIKE 'fresh\_valkey\_stream\_loss\_%' ESCAPE '\';

-- Runtime release envelopes are immutable and retain their original bytes and
-- digest. Normalize every relational provider/surface dimension in place.
ALTER TABLE prices DROP CONSTRAINT prices_provider_kind_check;
ALTER TABLE usage_facts DROP CONSTRAINT usage_facts_surface_check;
ALTER TABLE usage_hourly DROP CONSTRAINT usage_hourly_surface_check;
ALTER TABLE async_media_jobs DROP CONSTRAINT async_media_jobs_surface_check;

UPDATE providers
   SET kind = CASE kind
       WHEN 'open_ai' THEN 'openai'
       WHEN 'azure_open_ai' THEN 'azure_openai'
       WHEN 'open_ai_compatible' THEN 'openai_compatible'
       END
 WHERE kind IN ('open_ai', 'azure_open_ai', 'open_ai_compatible');

UPDATE model_capabilities SET surface = 'openai' WHERE surface = 'open_ai';

UPDATE prices
   SET provider_kind = CASE provider_kind
       WHEN 'open_ai' THEN 'openai'
       WHEN 'azure_open_ai' THEN 'azure_openai'
       WHEN 'open_ai_compatible' THEN 'openai_compatible'
       END
 WHERE provider_kind IN ('open_ai', 'azure_open_ai', 'open_ai_compatible');

UPDATE requests SET surface = 'openai' WHERE surface = 'open_ai';
UPDATE usage_facts SET surface = 'openai' WHERE surface = 'open_ai';
UPDATE usage_hourly SET surface = 'openai' WHERE surface = 'open_ai';
UPDATE async_media_jobs SET surface = 'openai' WHERE surface = 'open_ai';

UPDATE provider_revisions
   SET kind = CASE kind
       WHEN 'open_ai' THEN 'openai'
       WHEN 'azure_open_ai' THEN 'azure_openai'
       WHEN 'open_ai_compatible' THEN 'openai_compatible'
       END
 WHERE kind IN ('open_ai', 'azure_open_ai', 'open_ai_compatible');

UPDATE provider_revision_capabilities
   SET surface = 'openai'
 WHERE surface = 'open_ai';

UPDATE runtime_generation_provider_configs
   SET kind = CASE kind
       WHEN 'open_ai' THEN 'openai'
       WHEN 'azure_open_ai' THEN 'azure_openai'
       WHEN 'open_ai_compatible' THEN 'openai_compatible'
       END
 WHERE kind IN ('open_ai', 'azure_open_ai', 'open_ai_compatible');

ALTER TABLE prices
    ADD CONSTRAINT prices_provider_kind_check CHECK (
        provider_kind IN (
            'openai', 'anthropic', 'gemini', 'vertex_ai', 'bedrock',
            'azure_openai', 'openai_compatible'
        )
    );
ALTER TABLE usage_facts
    ADD CONSTRAINT usage_facts_surface_check
        CHECK (surface IN ('openai', 'anthropic', 'gemini', 'unknown'));
ALTER TABLE usage_hourly
    ADD CONSTRAINT usage_hourly_surface_check
        CHECK (surface IN ('openai', 'anthropic', 'gemini', 'unknown'));
ALTER TABLE async_media_jobs
    ADD CONSTRAINT async_media_jobs_surface_check
        CHECK (surface IN ('openai', 'anthropic', 'gemini'));
