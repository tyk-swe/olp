ALTER TABLE olp.projects ADD COLUMN end_user_policy jsonb
    CHECK (end_user_policy IS NULL OR jsonb_typeof(end_user_policy) = 'object');

-- These accounts contain digests only and exist independently of the current
-- policy, so lowering a limit or enabling one cannot erase accrued spend.
CREATE TABLE olp.end_user_accounts (
    id uuid PRIMARY KEY,
    api_key_id uuid REFERENCES olp.api_keys ON DELETE CASCADE,
    project_id uuid REFERENCES olp.projects ON DELETE CASCADE,
    end_user_digest text NOT NULL CHECK (end_user_digest ~ '^[0-9a-f]{64}$'),
    CHECK ((api_key_id IS NULL) <> (project_id IS NULL)),
    UNIQUE NULLS NOT DISTINCT (api_key_id, project_id, end_user_digest)
);
CREATE TABLE olp.end_user_cost_windows (
    account_id uuid NOT NULL REFERENCES olp.end_user_accounts ON DELETE CASCADE,
    window_kind text NOT NULL CHECK (window_kind IN ('day', 'month')),
    window_id bigint NOT NULL CHECK (window_id >= 0),
    accrued numeric(28,12) NOT NULL CHECK (accrued >= 0),
    unpriced_attempts bigint NOT NULL CHECK (unpriced_attempts >= 0),
    CHECK (window_kind = 'month' OR unpriced_attempts = 0),
    PRIMARY KEY (account_id, window_kind, window_id)
);

CREATE INDEX attempt_usage_facts_end_user ON olp.attempt_usage_facts (end_user_digest, observed_at) WHERE end_user_digest <> '';
CREATE INDEX attempt_usage_hourly_end_user ON olp.attempt_usage_hourly (end_user_digest, bucket) WHERE end_user_digest <> '';
