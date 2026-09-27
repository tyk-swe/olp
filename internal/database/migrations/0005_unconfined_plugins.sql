-- Unconfined plugins (ADR 0005). An unconfined plugin is a native executable
-- in the deployment's image, which OLP never stores: it records the name the
-- executable has in the unconfined plugin directory instead of a module, and
-- its digest identifies the plugin as a module's does. An owner permits it in
-- one step, which approves it. Permitting takes a recent authentication.
ALTER TABLE olp.plugins
    ALTER COLUMN module DROP NOT NULL,
    ADD COLUMN executable text CHECK (executable ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    ADD COLUMN size_bytes bigint;
UPDATE olp.plugins SET size_bytes = octet_length(module);
ALTER TABLE olp.plugins
    ALTER COLUMN size_bytes SET NOT NULL,
    ADD CONSTRAINT plugins_tier_check CHECK ((module IS NULL) = (executable IS NOT NULL));

ALTER TABLE olp.recent_auth
    DROP CONSTRAINT recent_auth_purpose_check,
    ADD CONSTRAINT recent_auth_purpose_check CHECK (purpose IN ('password_enrollment','oidc_link','oidc_unlink','plugin_permit'));
