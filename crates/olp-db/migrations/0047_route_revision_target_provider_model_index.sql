-- no-transaction
-- Same as 0046 for revision targets: runtime snapshot compilation and provider
-- disablement join route_revision_targets on provider_model_id.
CREATE INDEX CONCURRENTLY route_revision_targets_provider_model_idx
    ON route_revision_targets(provider_model_id);
