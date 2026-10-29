ALTER TABLE olp.recent_auth DROP CONSTRAINT recent_auth_purpose_check,
    ADD CONSTRAINT recent_auth_purpose_check CHECK (purpose IN ('password_enrollment','oidc_link','oidc_unlink','saml_link','saml_unlink','plugin_permit','mfa_manage','agent_delegation'));

ALTER TABLE olp.agent_delegations
    ADD COLUMN consent_digest bytea UNIQUE,
    ADD COLUMN consenting_user_id uuid REFERENCES olp.users(id);
