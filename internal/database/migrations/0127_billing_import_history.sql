ALTER TABLE olp.provider_billing_contexts
    ADD COLUMN imported_from date,
    ADD COLUMN imported_through date,
    ADD COLUMN last_success_at timestamptz;

CREATE TABLE olp.provider_billing_days (
    provider_id uuid NOT NULL REFERENCES olp.providers(id) ON DELETE CASCADE,
    day date NOT NULL,
    source text NOT NULL,
    scope jsonb,
    last_success_at timestamptz,
    PRIMARY KEY (provider_id, day)
);

CREATE INDEX provider_billing_days_day_idx ON olp.provider_billing_days (day);

WITH spans AS (
    SELECT bc.provider_id, bc.source, bc.scope, r.covered_from AS first_day, r.covered_through AS last_day,
           COALESCE(bi.last_success_at, bc.last_success_at) AS last_success_at
    FROM olp.provider_billing_revisions r
    JOIN olp.provider_billing_contexts bc ON bc.provider_id = r.provider_id AND bc.source = r.source
    LEFT JOIN olp.provider_billing_imports bi ON bi.provider_id = bc.provider_id
    WHERE r.covered_from IS NOT NULL
)
INSERT INTO olp.provider_billing_days (provider_id, day, source, scope, last_success_at)
SELECT s.provider_id, d.day::date, s.source, s.scope, s.last_success_at
FROM spans s
CROSS JOIN LATERAL generate_series(s.first_day::timestamp, s.last_day::timestamp, interval '1 day') AS d(day)
ON CONFLICT (provider_id, day) DO NOTHING;

INSERT INTO olp.provider_billing_days (provider_id, day, source, scope, last_success_at)
SELECT provider_id, day, MIN(source), NULL, MAX(fetched_at)
FROM olp.provider_invoiced_costs
GROUP BY provider_id, day
ON CONFLICT (provider_id, day) DO NOTHING;

UPDATE olp.provider_billing_imports bi
SET (imported_from, imported_through) = (
    SELECT lower(span), upper(span) - 1
    FROM (
        SELECT unnest(range_agg(daterange(day, day, '[]'))) AS span
        FROM olp.provider_billing_days bd
        WHERE bd.provider_id = bi.provider_id AND bd.source = bi.source AND bd.scope = bi.scope
    ) spans
    ORDER BY upper(span) DESC
    LIMIT 1
);
