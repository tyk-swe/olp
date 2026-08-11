-- Usage facts outlive request/attempt metadata by default. Preserve exact
-- ingestion reconciliation without retaining request metadata by moving the
-- usage foreign key to a minimal identity/timestamp anchor.
CREATE TABLE usage_request_anchors (
    request_id uuid NOT NULL,
    request_started_at timestamptz NOT NULL,
    PRIMARY KEY (request_id, request_started_at)
);

INSERT INTO usage_request_anchors (request_id, request_started_at)
SELECT id, started_at FROM requests
ON CONFLICT DO NOTHING;

ALTER TABLE usage_facts
    DROP CONSTRAINT usage_facts_request_id_request_started_at_fkey,
    ADD CONSTRAINT usage_facts_request_anchor_fkey
        FOREIGN KEY (request_id, request_started_at)
        REFERENCES usage_request_anchors(request_id, request_started_at)
        ON DELETE CASCADE;

CREATE INDEX usage_request_anchors_started_at_idx
    ON usage_request_anchors(request_started_at);

-- Pricing may be known while usage is incomplete (for example an upstream
-- omitted its terminal usage frame). Preserve that distinction: cost remains
-- NULL, `unpriced` remains false, and `usage_complete` explains why no estimate
-- is available.
DO $$
DECLARE
    cost_constraint text;
BEGIN
    SELECT conname INTO cost_constraint
    FROM pg_constraint
    WHERE conrelid = 'usage_facts'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%estimated_cost IS NULL%unpriced%';

    IF cost_constraint IS NOT NULL THEN
        EXECUTE format('ALTER TABLE usage_facts DROP CONSTRAINT %I', cost_constraint);
    END IF;
END $$;

ALTER TABLE usage_facts ADD CONSTRAINT usage_facts_cost_completeness_check CHECK (
    (estimated_cost IS NULL AND (unpriced OR NOT usage_complete))
    OR (estimated_cost IS NOT NULL AND NOT unpriced)
);
