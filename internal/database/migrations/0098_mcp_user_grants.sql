ALTER TABLE olp.mcp_server_revisions ADD COLUMN oauth jsonb;

CREATE TABLE olp.mcp_grant_enrollments (
 id uuid PRIMARY KEY,
 server_id uuid NOT NULL REFERENCES olp.mcp_servers,
 project_id uuid NOT NULL REFERENCES olp.projects,
 binding jsonb NOT NULL,
 binding_digest text NOT NULL CHECK(binding_digest ~ '^[a-f0-9]{64}$'),
 handoff_digest bytea NOT NULL CHECK(octet_length(handoff_digest)=32),
 started_by uuid REFERENCES olp.users,
 claimed_by uuid REFERENCES olp.users,
 claimed_at timestamptz,
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK((claimed_by IS NULL)=(claimed_at IS NULL))
);
CREATE INDEX mcp_grant_enrollments_project ON olp.mcp_grant_enrollments(project_id,created_at DESC,id DESC);

CREATE TABLE olp.mcp_user_grants (
 id uuid PRIMARY KEY,
 enrollment_id uuid NOT NULL UNIQUE REFERENCES olp.mcp_grant_enrollments,
 server_id uuid NOT NULL REFERENCES olp.mcp_servers,
 project_id uuid NOT NULL REFERENCES olp.projects,
 binding jsonb NOT NULL,
 binding_digest text NOT NULL UNIQUE CHECK(binding_digest ~ '^[a-f0-9]{64}$'),
 member_id uuid NOT NULL REFERENCES olp.users,
 access_secret_id uuid REFERENCES olp.secrets,
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mcp_user_grants_project ON olp.mcp_user_grants(project_id,created_at DESC,id DESC);

ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp.secrets ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN (
 'provider_credential','provider_continuation','mfa_totp','mfa_webauthn',
 'notification_secret','mutation_replay','oidc_client','oidc_flow',
 'media_job_source','provider_grant_refresh','grant_enrollment','saml_key','saml_flow',
 'sink_credential','mcp_credential','billing_credential','guardrail_credential',
 'provider_upload_session','response_cache','agent_credential','mcp_grant_access'
));
