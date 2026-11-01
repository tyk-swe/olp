ALTER TABLE olp.mcp_servers ADD COLUMN entra_client_secret_id uuid REFERENCES olp.secrets(id);
ALTER TABLE olp.mcp_user_grants ALTER COLUMN member_id DROP NOT NULL;
ALTER TABLE olp.mcp_user_grants ADD CONSTRAINT mcp_user_grants_member_origin CHECK (member_id IS NOT NULL OR binding->'oauth'->'entra_obo' IS NOT NULL);

ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp.secrets ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN (
 'provider_credential','provider_continuation','mfa_totp','mfa_webauthn',
 'notification_secret','mutation_replay','oidc_client','oidc_flow',
 'media_job_source','provider_grant_refresh','grant_enrollment','saml_key','saml_flow',
 'sink_credential','mcp_credential','billing_credential','guardrail_credential',
 'provider_upload_session','response_cache','agent_credential','mcp_grant_access',
 'mcp_grant_refresh','mcp_grant_enrollment','mcp_delegation_signing','mcp_entra_client'
));
