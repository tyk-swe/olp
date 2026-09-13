-- Keep bounded expiry cleanup and account authority checks efficient as the
-- installation grows. This forward migration also applies to populated M2 DBs.
CREATE INDEX sessions_expiry ON olp_go.sessions(expires_at, id);
CREATE INDEX secrets_expiry ON olp_go.secrets(expires_at, id) WHERE expires_at IS NOT NULL;
CREATE INDEX auth_admission_expiry ON olp_go.auth_admission(window_started_at);
CREATE INDEX invitations_email ON olp_go.invitations(email) WHERE accepted_at IS NULL AND revoked_at IS NULL;
