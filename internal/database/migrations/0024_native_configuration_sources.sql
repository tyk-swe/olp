-- Native configuration is source data, not a numeric/object-normalized index.
-- Convert the existing authoritative columns rather than adding a competing
-- mutable representation. Historic jsonb values remain exactly as they exist;
-- number spellings already normalized by jsonb cannot be reconstructed.
ALTER TABLE olp_go.providers
    ALTER COLUMN configuration TYPE json USING configuration::json;
ALTER TABLE olp_go.provider_revisions
    ALTER COLUMN configuration TYPE json USING configuration::json;
ALTER TABLE olp_go.runtime_releases
    ALTER COLUMN snapshot TYPE json USING snapshot::json;

COMMENT ON COLUMN olp_go.providers.configuration IS
    'Authoritative provider configuration; native JSON number spellings and subtrees are preserved.';
COMMENT ON COLUMN olp_go.provider_revisions.configuration IS
    'Immutable authoritative provider configuration at activation; never normalize through jsonb.';
COMMENT ON COLUMN olp_go.runtime_releases.snapshot IS
    'Authoritative serving snapshot. Preserve native JSON source and its recorded sha256; never rewrite a digest after normalization.';
