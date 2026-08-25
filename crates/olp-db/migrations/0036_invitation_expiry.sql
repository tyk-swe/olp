-- A timed-out invitation used to be freed from the pending-email index by
-- stamping revoked_at/revoked_by with whichever operator happened to invite
-- the same address next. That rewrote a passive timeout as deliberate operator
-- intent and attributed it to someone who never revoked anything. Record
-- expiry in its own column instead, leaving revocation to mean revocation.

ALTER TABLE invitations ADD COLUMN expired_at timestamptz;

ALTER TABLE invitations
    ADD CONSTRAINT invitations_expiry_not_accepted CHECK (
        NOT (accepted_at IS NOT NULL AND expired_at IS NOT NULL)
    );

DROP INDEX invitations_pending_email_idx;
CREATE UNIQUE INDEX invitations_pending_email_idx
    ON invitations(email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL AND expired_at IS NULL;
