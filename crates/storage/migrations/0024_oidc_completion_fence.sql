-- Bind every persisted authorization flow to the configuration revision that
-- created it. N-1 binaries omit this column and therefore fail closed after
-- the migration instead of inserting a redirect after an update invalidated
-- the old revision.
ALTER TABLE oidc_authorization_flows
    ADD COLUMN configuration_etag uuid;

UPDATE oidc_authorization_flows flow
SET configuration_etag = configuration.etag
FROM oidc_configurations configuration
WHERE configuration.id = flow.configuration_id;

ALTER TABLE oidc_authorization_flows
    ALTER COLUMN configuration_etag SET NOT NULL;

-- New completion code sets this transaction-local marker only while holding
-- the OIDC configuration lock and after verifying the exact enabled ETag.
-- Every OIDC login/link changes its identity row, so this database fence also
-- rolls back callbacks already consumed by an N-1 binary before an update.
CREATE FUNCTION enforce_oidc_completion_fence() RETURNS trigger AS $$
DECLARE
    checked_etag text;
BEGIN
    checked_etag := current_setting('olp.oidc_configuration_etag', true);
    IF checked_etag IS NULL OR NOT EXISTS (
        SELECT 1
        FROM oidc_configurations
        WHERE singleton
          AND enabled
          AND etag::text = checked_etag
    ) THEN
        RAISE EXCEPTION 'OIDC completion requires a current enabled configuration fence'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER oidc_identities_completion_guard
    BEFORE INSERT OR UPDATE ON oidc_identities
    FOR EACH ROW EXECUTE FUNCTION enforce_oidc_completion_fence();
