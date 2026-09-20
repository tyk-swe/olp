-- Credentials do not determine who manages authorization. Local provisioning
-- evidence preserves setup/invited members, including members who linked OIDC.
-- Ambiguous legacy mixed-method accounts follow OIDC (fail closed); operators
-- must review these accounts before upgrading. See docs/access.md.
ALTER TABLE olp_go.users
    ADD COLUMN role_management text NOT NULL DEFAULT 'local'
        CHECK (role_management IN ('local','oidc')),
    ADD COLUMN oidc_authorized boolean NOT NULL DEFAULT true;
UPDATE olp_go.users u SET role_management='oidc'
WHERE EXISTS (SELECT 1 FROM olp_go.oidc_identities i WHERE i.user_id=u.id)
  AND NOT EXISTS (SELECT 1 FROM olp_go.invitations i WHERE i.accepted_by=u.id)
  AND NOT EXISTS (SELECT 1 FROM olp_go.audit a WHERE a.actor_user_id=u.id
      AND a.action IN ('installation.setup','invitation.accept') AND a.outcome='success');

-- A coarse display hint, never authentication evidence. Existing sessions have
-- unknown metadata; no raw user agent or address is retained here.
ALTER TABLE olp_go.sessions ADD COLUMN browser_hint text NOT NULL DEFAULT 'Unknown browser';
