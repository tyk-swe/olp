ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp.secrets
    ADD CONSTRAINT secrets_purpose_check
        CHECK (purpose IN ('oidc_client', 'oidc_flow', 'mutation_replay',
                           'provider_credential', 'notification_secret'));
CREATE TABLE olp.notification_destinations (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    url text NOT NULL,
    project_id uuid REFERENCES olp.projects,
    secret_id uuid REFERENCES olp.secrets ON DELETE SET NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX notification_destinations_name_scope
    ON olp.notification_destinations
    (lower(name), COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid));
CREATE TABLE olp.budget_alert_rules (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    project_id uuid REFERENCES olp.projects,
    subject_kind text NOT NULL CHECK (subject_kind IN ('api_key', 'budget_group')),
    subject_id uuid NOT NULL,
    window_kind text NOT NULL CHECK (window_kind IN ('day', 'month')),
    threshold_percent integer NOT NULL CHECK (threshold_percent BETWEEN 1 AND 100),
    destination_id uuid NOT NULL REFERENCES olp.notification_destinations,
    enabled boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (subject_kind, subject_id, window_kind, threshold_percent, destination_id)
);
CREATE TABLE olp.budget_alert_deliveries (
    id uuid PRIMARY KEY,
    rule_id uuid NOT NULL REFERENCES olp.budget_alert_rules,
    window_id bigint NOT NULL,
    threshold_percent integer NOT NULL,
    accrued numeric(28,12) NOT NULL CHECK (accrued >= 0),
    limit_amount numeric(24,12) NOT NULL CHECK (limit_amount > 0),
    currency char(3),
    status text NOT NULL CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5),
    last_error_code text,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_attempt_at timestamptz,
    delivered_at timestamptz,
    UNIQUE (rule_id, window_id, threshold_percent)
);
ALTER TABLE olp.worker_task_health DROP CONSTRAINT worker_task_health_task_check;
ALTER TABLE olp.worker_task_health
    ADD CONSTRAINT worker_task_health_task_check
        CHECK (task IN ('request_metadata_consumer', 'maintenance',
                        'cost_reconciliation',
                        'request_metadata_gateway_epoch_detection',
                        'media_reconciliation', 'budget_alert_delivery'));
