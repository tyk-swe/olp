-- A grant lapses when it can no longer be refreshed (ADR 0006): its plugin
-- reports that the upstream won't refresh it, or a refresh authorizes another
-- account than the grant's. Lapse is terminal: the grant's refresh token is
-- discarded, its credential version's slots are ineligible, and only a new
-- grant enrollment, which creates another credential version, replaces it.
ALTER TABLE olp.provider_grants
    ADD COLUMN lapsed_at timestamptz,
    ADD CONSTRAINT provider_grants_lapsed_check
        CHECK (lapsed_at IS NULL OR (refresh_token_id IS NULL AND refresh_at IS NULL));
