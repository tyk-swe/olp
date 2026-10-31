ALTER TABLE olp.provider_billing_contexts
    ADD COLUMN imported_from date,
    ADD COLUMN imported_through date,
    ADD COLUMN last_success_at timestamptz;
