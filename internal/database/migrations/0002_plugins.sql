-- Provider plugins (ADR 0005). A plugin is stored by the SHA-256 digest of its
-- module, with the manifest the module declared at install. It can't be used
-- until an owner approves the origins that manifest declares; approval covers
-- exactly those origins, which never change for a digest.
CREATE TABLE olp.plugins (
    digest text PRIMARY KEY CHECK (digest ~ '^[0-9a-f]{64}$'),
    abi_version integer NOT NULL,
    manifest jsonb NOT NULL,
    module bytea NOT NULL,
    etag uuid NOT NULL,
    installed_by uuid NOT NULL REFERENCES olp.users,
    installed_at timestamptz NOT NULL DEFAULT now(),
    approved_by uuid REFERENCES olp.users,
    approved_at timestamptz,
    CONSTRAINT plugins_approval_check CHECK ((approved_by IS NULL) = (approved_at IS NULL))
);
