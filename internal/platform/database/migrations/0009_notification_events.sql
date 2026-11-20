-- Notification rules subscribe a destination to an event: a budget threshold,
-- or a provider event, such as a grant lapse (ADR 0008), which concerns the
-- whole installation. The worker that delivered budget alerts delivers every
-- event, with the same signing, retries and backoff.
ALTER TABLE olp.budget_alert_rules RENAME TO notification_rules;
ALTER TABLE olp.notification_rules
    ADD COLUMN event text NOT NULL DEFAULT 'budget.threshold'
        CHECK (event IN ('budget.threshold', 'provider.grant.lapsed')),
    ALTER COLUMN subject_kind DROP NOT NULL,
    ALTER COLUMN subject_id DROP NOT NULL,
    ALTER COLUMN window_kind DROP NOT NULL,
    ALTER COLUMN threshold_percent DROP NOT NULL,
    -- A budget threshold rule names its subject, window and threshold. A
    -- provider event rule names none and is installation-wide.
    ADD CONSTRAINT notification_rules_subject_check CHECK (CASE event
        WHEN 'budget.threshold' THEN num_nonnulls(subject_kind, subject_id, window_kind, threshold_percent) = 4
        ELSE project_id IS NULL AND num_nonnulls(subject_kind, subject_id, window_kind, threshold_percent) = 0 END);
ALTER TABLE olp.notification_rules ALTER COLUMN event DROP DEFAULT;
-- A destination is subscribed to a provider event once.
CREATE UNIQUE INDEX notification_rules_provider_event
    ON olp.notification_rules (event, destination_id) WHERE event <> 'budget.threshold';

-- A delivery records its event's evidence: a budget threshold's spend as it
-- was claimed, or a lapsed grant's credential version with the payload that
-- reports the lapse as it was when the grant lapsed.
ALTER TABLE olp.budget_alert_deliveries RENAME TO notification_deliveries;
ALTER TABLE olp.notification_deliveries
    ALTER COLUMN window_id DROP NOT NULL,
    ALTER COLUMN threshold_percent DROP NOT NULL,
    ALTER COLUMN accrued DROP NOT NULL,
    ALTER COLUMN limit_amount DROP NOT NULL,
    ADD COLUMN credential_id uuid REFERENCES olp.provider_credentials ON DELETE CASCADE,
    ADD COLUMN payload jsonb,
    ADD CONSTRAINT notification_deliveries_evidence_check CHECK (
        num_nonnulls(window_id, threshold_percent, accrued, limit_amount) = 4 AND num_nonnulls(credential_id, payload) = 0
        OR num_nonnulls(window_id, threshold_percent, accrued, limit_amount, currency) = 0 AND num_nonnulls(credential_id, payload) = 2),
    -- A grant lapses once, and each rule is sent one delivery of it.
    ADD CONSTRAINT notification_deliveries_lapse_key UNIQUE (rule_id, credential_id);

ALTER TABLE olp.worker_task_health DROP CONSTRAINT worker_task_health_task_check;
UPDATE olp.worker_task_health SET task = 'notification_delivery' WHERE task = 'budget_alert_delivery';
ALTER TABLE olp.worker_task_health
    ADD CONSTRAINT worker_task_health_task_check CHECK (task IN ('request_metadata_consumer', 'maintenance',
        'cost_reconciliation', 'request_metadata_gateway_epoch_detection', 'media_reconciliation',
        'notification_delivery', 'grant_refresh'));
