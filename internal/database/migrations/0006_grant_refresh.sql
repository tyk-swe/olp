-- Workers refresh grants ahead of their access tokens' expiry (ADR 0008).
--
-- A refresh rewrites the grant's credential version's provider_credential
-- secret with the new access token and advances the grant's generation, which
-- gateways poll to reload just the access tokens that changed.
ALTER TABLE olp.provider_grants
    ADD COLUMN generation bigint NOT NULL DEFAULT 1,
    -- When a worker refreshes the grant next: ahead of its access token's
    -- expiry, after a backoff when a refresh failed, or at once when a gateway
    -- saw the upstream refuse the access token. NULL when nothing is scheduled:
    -- the upstream did not say when the access token expires, or the grant has
    -- no refresh token.
    ADD COLUMN refresh_at timestamptz,
    -- The refreshes that failed since the last one that succeeded, and why the
    -- last of them failed. A permanent failure also ends the grant's refresh
    -- token.
    ADD COLUMN refresh_failures integer NOT NULL DEFAULT 0 CHECK (refresh_failures >= 0),
    ADD COLUMN refresh_failure text;

-- Grants enrolled before workers refreshed them are refreshed at once.
UPDATE olp.provider_grants SET refresh_at = now() WHERE refresh_token_id IS NOT NULL;

CREATE INDEX provider_grants_refresh ON olp.provider_grants(refresh_at) WHERE refresh_token_id IS NOT NULL;

ALTER TABLE olp.worker_task_health
    DROP CONSTRAINT worker_task_health_task_check,
    ADD CONSTRAINT worker_task_health_task_check CHECK (task IN ('request_metadata_consumer', 'maintenance',
        'cost_reconciliation', 'request_metadata_gateway_epoch_detection', 'media_reconciliation',
        'budget_alert_delivery', 'grant_refresh'));
