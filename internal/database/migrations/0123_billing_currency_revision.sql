ALTER TABLE olp.provider_invoiced_costs
    DROP CONSTRAINT provider_invoiced_costs_pkey,
    ADD PRIMARY KEY (provider_id, day, line_item, currency);

ALTER TABLE olp.provider_invoiced_costs
    ALTER COLUMN amount TYPE numeric;

ALTER TABLE olp.provider_invoiced_costs
    ADD COLUMN source_revision text;

ALTER TABLE olp.provider_billing_imports
    ADD COLUMN source_revision text;

CREATE TABLE olp.provider_billing_revisions (
    provider_id uuid NOT NULL REFERENCES olp.providers ON DELETE CASCADE,
    source text NOT NULL,
    scope_digest bytea NOT NULL,
    period_start date NOT NULL,
    source_revision text NOT NULL,
    ordering_at timestamptz NOT NULL,
    covered_from date,
    covered_through date,
    PRIMARY KEY (provider_id, source, scope_digest, period_start)
);

CREATE TABLE olp.provider_billing_contexts (
    provider_id uuid PRIMARY KEY REFERENCES olp.providers ON DELETE CASCADE,
    source text NOT NULL,
    scope jsonb NOT NULL
);

INSERT INTO olp.provider_billing_contexts(provider_id, source, scope)
SELECT provider_id, source, scope FROM olp.provider_billing_imports;
