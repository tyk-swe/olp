-- Complete the local identity-management lifecycle without changing the V2
-- fresh-install contract introduced by 0001.

ALTER TABLE users
    ADD COLUMN etag uuid NOT NULL DEFAULT gen_random_uuid();

UPDATE invitations SET email = lower(btrim(email));

ALTER TABLE invitations
    ADD COLUMN accepted_by uuid REFERENCES users(id),
    ADD COLUMN revoked_at timestamptz,
    ADD COLUMN revoked_by uuid REFERENCES users(id),
    ADD CONSTRAINT invitations_email_normalized CHECK (email = lower(btrim(email))),
    ADD CONSTRAINT invitations_lifecycle_exclusive CHECK (
        NOT (accepted_at IS NOT NULL AND revoked_at IS NOT NULL)
    ),
    ADD CONSTRAINT invitations_acceptance_complete CHECK (
        (accepted_at IS NULL) = (accepted_by IS NULL)
    ),
    ADD CONSTRAINT invitations_revocation_complete CHECK (
        (revoked_at IS NULL) = (revoked_by IS NULL)
    ),
    ADD CONSTRAINT invitations_expiry_after_creation CHECK (expires_at > created_at);

CREATE UNIQUE INDEX invitations_pending_email_idx
    ON invitations(email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;
CREATE INDEX invitations_created_at_id_idx ON invitations(created_at DESC, id DESC);
CREATE INDEX sessions_created_at_id_idx ON sessions(created_at DESC, id DESC);
CREATE INDEX users_created_at_id_idx ON users(created_at DESC, id DESC);
