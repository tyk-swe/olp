-- Operation/dialect admission belongs to the trusted application registry. SQL
-- retains bounded opaque labels so adding a codec does not require a schema edit.
ALTER TABLE olp_go.prices DROP CONSTRAINT prices_operation_check;
ALTER TABLE olp_go.prices ADD CONSTRAINT prices_operation_check
    CHECK (operation ~ '^[a-z][a-z0-9_-]{0,95}$');
ALTER TABLE olp_go.attempt_usage_facts DROP CONSTRAINT attempt_usage_facts_surface_check;
ALTER TABLE olp_go.attempt_usage_facts ADD CONSTRAINT attempt_usage_facts_surface_check
    CHECK (surface ~ '^[a-z][a-z0-9_-]{0,95}$');
ALTER TABLE olp_go.attempt_usage_hourly DROP CONSTRAINT attempt_usage_hourly_surface_check;
ALTER TABLE olp_go.attempt_usage_hourly ADD CONSTRAINT attempt_usage_hourly_surface_check
    CHECK (surface ~ '^[a-z][a-z0-9_-]{0,95}$');
