-- An identity provider role sync demoting a user revokes their pending
-- invitations without an initiating operator. Attributing that revocation
-- to the demoted user rewrote an IdP-driven transition as self-revocation.
-- Allow a NULL revoked_by while continuing to require revoked_at whenever
-- an actor is recorded.

ALTER TABLE invitations DROP CONSTRAINT invitations_revocation_complete;
ALTER TABLE invitations ADD CONSTRAINT invitations_revocation_complete
    CHECK (revoked_by IS NULL OR revoked_at IS NOT NULL);
