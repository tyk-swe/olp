ALTER TABLE olp.mcp_grant_enrollments
 ADD COLUMN flow text CHECK(flow IN ('authorization_code','device_code')),
 ADD COLUMN session_secret_id uuid REFERENCES olp.secrets,
 ADD COLUMN exchange_attempt_id uuid,
 ADD COLUMN outcome text CHECK(outcome IN ('completed','failed','denied','expired')),
 ADD COLUMN poll_at timestamptz,
 ADD COLUMN poll_interval integer CHECK(poll_interval BETWEEN 1 AND 300);

ALTER TABLE olp.mcp_user_grants
 ADD COLUMN refresh_secret_id uuid REFERENCES olp.secrets,
 ADD COLUMN refresh_attempt_id uuid,
 ADD COLUMN failure_code text CHECK(failure_code IN ('refresh_failed'));

ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp.secrets ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN (
 'provider_credential','provider_continuation','mfa_totp','mfa_webauthn',
 'notification_secret','mutation_replay','oidc_client','oidc_flow',
 'media_job_source','provider_grant_refresh','grant_enrollment','saml_key','saml_flow',
 'sink_credential','mcp_credential','billing_credential','guardrail_credential',
 'provider_upload_session','response_cache','agent_credential','mcp_grant_access',
 'mcp_grant_refresh','mcp_grant_enrollment'
));
