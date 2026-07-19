-- Raw usage facts are retention-bound, but Valkey delivery is at least once
-- and a pending entry can outlive that retention window. Keep a minimal,
-- seven-day identity ledger so replaying an already-rolled event cannot add
-- its tokens and cost to hourly aggregates a second time. Events outside that
-- delivery contract are fenced below and surfaced as explicit gaps.
CREATE TYPE usage_event_receipt_status AS ENUM
    ('pending', 'fact_persisted', 'rejected');

CREATE TABLE usage_event_receipts (
    event_id uuid PRIMARY KEY,
    request_id uuid NOT NULL UNIQUE,
    event_sha256 bytea,
    status usage_event_receipt_status NOT NULL,
    observed_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    CHECK (event_sha256 IS NULL OR octet_length(event_sha256) = 32)
);

-- Receipts are append-oriented. BRIN keeps the retention index small even at
-- high request rates while still allowing efficient oldest-first cleanup.
CREATE INDEX usage_event_receipts_recorded_at_idx
    ON usage_event_receipts USING brin(recorded_at);

-- Stream poison handling commits before XACK/XDEL. A durable source key makes
-- that gap write idempotent across a crash or Valkey acknowledgement failure.
ALTER TABLE usage_ingestion_gaps
    ADD COLUMN deduplication_key text,
    ADD CHECK (deduplication_key IS NULL OR
               (deduplication_key <> '' AND octet_length(deduplication_key) <= 256));
CREATE UNIQUE INDEX usage_ingestion_gaps_deduplication_key_idx
    ON usage_ingestion_gaps(deduplication_key)
    WHERE deduplication_key IS NOT NULL;

-- This trigger also fences an N-1 worker during a rolling upgrade: old code
-- does not consult the receipt table, but PostgreSQL still suppresses a fact
-- whose durable identity was already accepted.
CREATE FUNCTION enforce_usage_fact_receipt() RETURNS trigger AS $$
BEGIN
    UPDATE usage_event_receipts
    SET status = 'fact_persisted'
    WHERE event_id = NEW.id
      AND request_id = NEW.request_id
      AND status = 'pending';
    IF FOUND THEN
        RETURN NEW;
    END IF;

    IF EXISTS (
        SELECT 1 FROM usage_event_receipts
        WHERE event_id = NEW.id
          AND request_id = NEW.request_id
          AND status IN ('fact_persisted', 'rejected')
    ) THEN
        RETURN NULL;
    END IF;

    -- A fact that is still raw is already its own exact identity ledger. This
    -- avoids reporting a false gap for an N-1 worker replaying a pre-migration
    -- fact that did not need a receipt backfill.
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

    -- Bound deduplication state. An N-1 worker does not perform the matching
    -- application validation, so suppress an out-of-contract first delivery
    -- in the database and make the resulting loss visible rather than adding
    -- it to an already-retained hour. The interval covers both the claimed
    -- observation and detection time because the rejected timestamp is not a
    -- trusted completeness boundary.
    IF NEW.observed_at < now() - interval '7 days'
       OR NEW.observed_at > now() + interval '5 minutes' THEN
        INSERT INTO usage_event_receipts
            (event_id, request_id, event_sha256, status, observed_at)
        VALUES (NEW.id, NEW.request_id, NULL, 'rejected', NEW.observed_at)
        ON CONFLICT DO NOTHING;
        IF FOUND THEN
            INSERT INTO usage_ingestion_gaps
                (id, gateway_instance, event_count, reason, certainty,
                 first_observed_at, last_observed_at)
            VALUES
                (gen_random_uuid(), 'database-fence', 0,
                 'usage_event_outside_replay_window',
                 'lower_bound'::usage_gap_certainty, now(), now());
        END IF;
        RETURN NULL;
    END IF;

    INSERT INTO usage_event_receipts
        (event_id, request_id, event_sha256, status, observed_at)
    VALUES (NEW.id, NEW.request_id, NULL, 'fact_persisted', NEW.observed_at);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER usage_facts_receipt_guard
    BEFORE INSERT ON usage_facts
    FOR EACH ROW EXECUTE FUNCTION enforce_usage_fact_receipt();

-- Existing raw facts are already exact identity records. Preserve only the
-- compact identity immediately before a fact disappears, avoiding a blocking
-- multi-million-row upgrade backfill. This also covers an N-1 maintenance
-- transaction: the receipt and fact deletion commit atomically.
CREATE FUNCTION preserve_usage_fact_receipt() RETURNS trigger AS $$
BEGIN
    IF OLD.observed_at >= now() - interval '7 days' THEN
        INSERT INTO usage_event_receipts
            (event_id, request_id, event_sha256, status, observed_at)
        VALUES (OLD.id, OLD.request_id, NULL, 'fact_persisted', OLD.observed_at)
        ON CONFLICT DO NOTHING;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER usage_facts_preserve_receipt
    BEFORE DELETE ON usage_facts
    FOR EACH ROW EXECUTE FUNCTION preserve_usage_fact_receipt();

-- Old workers used replacement semantics when an hourly bucket already
-- existed. After this migration, only the additive maintenance implementation
-- may write retained aggregates; an N-1 pass fails and rolls back its fact
-- deletion instead of overwriting prior totals.
CREATE FUNCTION enforce_usage_rollup_writer() RETURNS trigger AS $$
BEGIN
    IF current_setting('olp.usage_rollup_writer', true) IS DISTINCT FROM 'additive-v2' THEN
        RAISE EXCEPTION 'usage rollup requires the additive writer fence'
            USING ERRCODE = '55000';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER usage_hourly_writer_guard
    BEFORE INSERT OR UPDATE ON usage_hourly
    FOR EACH STATEMENT EXECUTE FUNCTION enforce_usage_rollup_writer();

-- Fence the gap aggregate independently as well. The current maintenance
-- path writes both aggregate tables with the same transaction-local marker,
-- while an older binary cannot aggregate then delete raw gap evidence.
CREATE TRIGGER usage_gap_hourly_writer_guard
    BEFORE INSERT OR UPDATE ON usage_gap_hourly
    FOR EACH STATEMENT EXECUTE FUNCTION enforce_usage_rollup_writer();
