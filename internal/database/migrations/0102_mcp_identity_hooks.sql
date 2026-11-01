ALTER TABLE olp.mcp_servers ADD COLUMN delegation_key_id uuid REFERENCES olp.secrets(id) ON DELETE SET NULL;
ALTER TABLE olp.mcp_server_revisions ADD COLUMN delegation jsonb CHECK(delegation IS NULL OR jsonb_typeof(delegation)='object');
ALTER TABLE olp.mcp_tool_calls ADD COLUMN identity_decisions jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(identity_decisions)='array' AND octet_length(identity_decisions::text)<=4096);
ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp.secrets ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN (
 'provider_credential','provider_continuation','mfa_totp','mfa_webauthn',
 'notification_secret','mutation_replay','oidc_client','oidc_flow',
 'media_job_source','provider_grant_refresh','grant_enrollment','saml_key','saml_flow',
 'sink_credential','mcp_credential','billing_credential','guardrail_credential',
 'provider_upload_session','response_cache','agent_credential','mcp_grant_access',
 'mcp_grant_refresh','mcp_grant_enrollment','mcp_delegation_signing'
));
