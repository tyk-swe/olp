ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check,
 ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN ('oidc_client','oidc_flow','mutation_replay','provider_credential','notification_secret','provider_continuation','media_job_source','provider_grant_refresh','grant_enrollment','mfa_totp','mfa_webauthn'));
ALTER TABLE olp.users ADD COLUMN mfa_revision uuid NOT NULL DEFAULT uuidv7();
ALTER TABLE olp.sessions ADD COLUMN auth_method text NOT NULL DEFAULT 'local' CHECK(auth_method IN ('local','oidc','saml')),
 ADD COLUMN mfa_verified boolean NOT NULL DEFAULT false;
ALTER TABLE olp.recent_auth DROP CONSTRAINT recent_auth_purpose_check,
 ADD CONSTRAINT recent_auth_purpose_check CHECK(purpose IN ('password_enrollment','oidc_link','oidc_unlink','plugin_permit','mfa_manage'));
INSERT INTO olp.settings(key,value,etag,updated_by)
 SELECT 'auth.mfa_required','false',uuidv7(),id FROM olp.users WHERE role='owner' ORDER BY id LIMIT 1 ON CONFLICT DO NOTHING;
CREATE TABLE olp.mfa_factors (
 id uuid PRIMARY KEY REFERENCES olp.secrets(id), user_id uuid NOT NULL REFERENCES olp.users(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK(kind IN ('totp','webauthn')), name text NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 credential_id bytea UNIQUE CHECK(credential_id IS NULL OR octet_length(credential_id) BETWEEN 1 AND 1024),
 last_counter bigint NOT NULL DEFAULT -1,created_at timestamptz NOT NULL DEFAULT now(),last_used_at timestamptz,
 CHECK((kind='webauthn')=(credential_id IS NOT NULL))
);
CREATE UNIQUE INDEX mfa_one_totp ON olp.mfa_factors(user_id) WHERE kind='totp';
CREATE INDEX mfa_user_factors ON olp.mfa_factors(user_id);
CREATE TABLE olp.mfa_recovery_codes (
 user_id uuid NOT NULL REFERENCES olp.users(id) ON DELETE CASCADE,digest bytea NOT NULL CHECK(octet_length(digest)=32),
 PRIMARY KEY(user_id,digest)
);
CREATE TABLE olp.mfa_challenges (
 id uuid PRIMARY KEY, digest bytea NOT NULL UNIQUE CHECK(octet_length(digest)=32),
 user_id uuid NOT NULL REFERENCES olp.users(id) ON DELETE CASCADE,
 user_etag uuid NOT NULL,mfa_revision uuid NOT NULL,
 session_id uuid REFERENCES olp.sessions(id) ON DELETE CASCADE,
 purpose text NOT NULL CHECK(purpose IN ('login','bootstrap','manage','reauth','enroll_totp','enroll_webauthn')),
 data jsonb NOT NULL CHECK(jsonb_typeof(data)='object'),
 secret_id uuid REFERENCES olp.secrets(id) ON DELETE CASCADE,attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5),
 expires_at timestamptz NOT NULL,created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mfa_challenge_expiry ON olp.mfa_challenges(expires_at);
CREATE INDEX mfa_challenge_user ON olp.mfa_challenges(user_id,created_at);
