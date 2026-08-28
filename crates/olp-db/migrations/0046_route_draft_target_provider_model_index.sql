-- no-transaction
-- Route draft targets reference provider models through a foreign key with no
-- supporting index, so provider disablement checks and draft revalidation
-- scanned every draft target. See 0040 for why each build is its own migration.
CREATE INDEX CONCURRENTLY route_draft_targets_provider_model_idx
    ON route_draft_targets(provider_model_id);
