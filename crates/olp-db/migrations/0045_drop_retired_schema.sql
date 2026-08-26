-- Schema drops staged in 0038 and 0039 for retirement in a follow-up release.
-- With the rollout complete, no active replicas write to these columns or
-- tables, so this release lands the drops.

ALTER TABLE oidc_authorization_flows DROP COLUMN client_digest;

DROP TABLE request_metadata_loss_reporter_state;

ALTER TABLE attempt_usage_facts
    DROP COLUMN attempt_started_at,
    DROP COLUMN attempt_completed_at;
