ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check,
 ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN ('oidc_client','oidc_flow','mutation_replay','provider_credential','notification_secret','provider_continuation','media_job_source','provider_grant_refresh','grant_enrollment','mfa_totp','mfa_webauthn','saml_key','saml_flow'));
ALTER TABLE olp.users DROP CONSTRAINT users_role_management_check,
 ADD CONSTRAINT users_role_management_check CHECK(role_management IN ('local','oidc','saml','provisioned'));
ALTER TABLE olp.recent_auth DROP CONSTRAINT recent_auth_purpose_check,
 ADD CONSTRAINT recent_auth_purpose_check CHECK(purpose IN ('password_enrollment','oidc_link','oidc_unlink','saml_link','saml_unlink','plugin_permit','mfa_manage'));
CREATE TABLE olp.saml_configuration (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),id uuid NOT NULL UNIQUE,
 document jsonb NOT NULL CHECK(jsonb_typeof(document)='object'),etag uuid NOT NULL,
 updated_by uuid NOT NULL REFERENCES olp.users,updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE olp.saml_identities (
 id uuid PRIMARY KEY,user_id uuid NOT NULL REFERENCES olp.users,issuer text NOT NULL,subject text NOT NULL,
 email_at_link text NOT NULL,role_claims jsonb,created_at timestamptz NOT NULL DEFAULT now(),last_login_at timestamptz,
 UNIQUE(issuer,subject)
);
CREATE INDEX saml_user_identities ON olp.saml_identities(user_id);
CREATE TABLE olp.saml_flows (
 id uuid PRIMARY KEY REFERENCES olp.secrets ON DELETE CASCADE,state_digest bytea NOT NULL UNIQUE,
 cookie_digest bytea NOT NULL,configuration_etag uuid NOT NULL,request_id text NOT NULL UNIQUE,
 received_at timestamptz,expires_at timestamptz NOT NULL
);
CREATE INDEX saml_flow_expiry ON olp.saml_flows(expires_at);
CREATE TABLE olp.saml_assertion_replays (
 digest bytea PRIMARY KEY CHECK(octet_length(digest)=32),expires_at timestamptz NOT NULL
);
CREATE INDEX saml_assertion_expiry ON olp.saml_assertion_replays(expires_at);
