ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp.secrets ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN ('saml_key','saml_flow','provider_credential','provider_continuation','mfa_totp','mfa_webauthn','notification_secret','sink_credential','agent_credential','mcp_credential','mcp_grant_access','mcp_grant_refresh','mcp_entra_client','mcp_delegation_signing','mcp_grant_enrollment','mutation_replay','oidc_client','oidc_flow','media_job_source','provider_grant_refresh','grant_enrollment','billing_credential','guardrail_credential','response_cache','provider_upload_session','code_continuation','workflow_continuation','code_summary_key'));
ALTER TABLE olp.secrets ADD CONSTRAINT secrets_code_summary_key_bound CHECK (purpose <> 'code_summary_key' OR (expires_at IS NULL AND octet_length(ciphertext)=60));
CREATE TABLE olp.code_summary_keys (
 id uuid PRIMARY KEY REFERENCES olp.secrets(id),
 root_id uuid NOT NULL REFERENCES olp.code_bindings(id),
 account_id uuid NOT NULL REFERENCES olp.code_accounts(id),
 principal text NOT NULL,
 UNIQUE(root_id,account_id,principal)
);
