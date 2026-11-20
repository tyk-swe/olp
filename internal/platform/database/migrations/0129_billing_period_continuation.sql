ALTER TABLE olp.provider_billing_imports
    ADD COLUMN cloud_next_period text,
    ADD COLUMN cloud_periods_remaining integer NOT NULL DEFAULT 0;
