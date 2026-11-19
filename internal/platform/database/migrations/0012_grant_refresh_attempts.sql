-- Commit a refresh attempt before dispatching its token. Losing the worker's
-- advisory-lock session must not authorize another use of that token.
-- While an attempt is in flight, refresh_at is its recovery deadline: after
-- that deadline, a worker lapses the grant instead of retrying a token whose
-- outcome was lost. Completion must still own this attempt.
ALTER TABLE olp.provider_grants
    ADD COLUMN refresh_attempt_id uuid,
    ADD CONSTRAINT provider_grants_refresh_attempt_check CHECK (
        refresh_attempt_id IS NULL OR (refresh_token_id IS NOT NULL AND refresh_at IS NOT NULL));
