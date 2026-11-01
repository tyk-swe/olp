ALTER TABLE olp.provider_billing_imports ADD COLUMN google_next_period date;
ALTER TABLE olp.provider_billing_imports ADD COLUMN google_period_phase smallint NOT NULL DEFAULT 0;
