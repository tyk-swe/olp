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
    provider_id uuid NOT NULL REFERENCES olp.provider_billing_imports ON DELETE CASCADE,
    period_start date NOT NULL,
    source_revision text NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (provider_id, period_start)
);
