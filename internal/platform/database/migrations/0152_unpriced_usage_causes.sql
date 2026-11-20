ALTER TABLE olp.attempt_usage_facts ADD COLUMN unpriced_cause text
    CHECK (unpriced_cause IN ('no_applicable_price', 'missing_price_component', 'incomplete_usage', 'unknown'));

ALTER TABLE olp.attempt_usage_hourly
    ADD COLUMN no_applicable_price_count bigint NOT NULL DEFAULT 0,
    ADD COLUMN missing_price_component_count bigint NOT NULL DEFAULT 0,
    ADD COLUMN incomplete_usage_unpriced_count bigint NOT NULL DEFAULT 0,
    ADD COLUMN unknown_unpriced_cause_count bigint NOT NULL DEFAULT 0;
