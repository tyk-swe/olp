ALTER TABLE olp.provider_billing_imports
    DROP COLUMN cloud_next_period,
    DROP COLUMN cloud_periods_remaining,
    DROP COLUMN google_next_period,
    DROP COLUMN google_period_phase,
    ADD COLUMN resume_period date,
    ADD COLUMN resume_remaining integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT provider_billing_imports_resume_check
        CHECK ((resume_period IS NULL AND resume_remaining = 0) OR (resume_period IS NOT NULL AND resume_remaining > 0));
