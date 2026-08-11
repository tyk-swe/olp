-- Make every authenticated browser session a snapshot of the user's durable
-- security state. Credential, role, active-state, and authentication-method
-- transitions advance the user version; stale bearers then fail closed even
-- if a future code path omits eager session deletion.
ALTER TABLE users
    ADD COLUMN security_version bigint NOT NULL DEFAULT 1
        CHECK (security_version > 0);

ALTER TABLE sessions
    ADD COLUMN security_version bigint;

UPDATE sessions session
   SET security_version = users.security_version
  FROM users
 WHERE users.id = session.user_id;

ALTER TABLE sessions
    ALTER COLUMN security_version SET NOT NULL,
    ADD CONSTRAINT sessions_security_version_positive CHECK (security_version > 0),
    ADD COLUMN recent_auth_token_digest bytea,
    ADD COLUMN recent_auth_purpose text,
    ADD COLUMN recent_auth_resource_id uuid,
    ADD COLUMN recent_auth_expires_at timestamptz,
    ADD CONSTRAINT sessions_recent_auth_complete CHECK (
        (
            recent_auth_token_digest IS NULL
            AND recent_auth_purpose IS NULL
            AND recent_auth_resource_id IS NULL
            AND recent_auth_expires_at IS NULL
        ) OR (
            recent_auth_token_digest IS NOT NULL
            AND octet_length(recent_auth_token_digest) = 32
            AND recent_auth_purpose IN (
                'password_enrollment', 'oidc_link', 'oidc_unlink'
            )
            AND recent_auth_expires_at IS NOT NULL
            AND (
                (recent_auth_purpose = 'oidc_unlink' AND recent_auth_resource_id IS NOT NULL)
                OR (recent_auth_purpose <> 'oidc_unlink' AND recent_auth_resource_id IS NULL)
            )
        )
    );

CREATE INDEX sessions_security_version_idx
    ON sessions(user_id, security_version);
CREATE INDEX sessions_recent_auth_expiry_idx
    ON sessions(recent_auth_expires_at)
    WHERE recent_auth_token_digest IS NOT NULL;

-- Persisted authorization flows are only ten minutes old and contain no
-- authorization result. Invalidating them avoids trying to infer an exact
-- initiating session/security version for rows written by an older binary.
DELETE FROM oidc_authorization_flows;

ALTER TABLE oidc_authorization_flows
    DROP CONSTRAINT oidc_authorization_flows_purpose_check,
    DROP CONSTRAINT oidc_authorization_flows_check,
    DROP CONSTRAINT oidc_authorization_flows_login_client,
    ADD COLUMN actor_session_id uuid REFERENCES sessions(id) ON DELETE CASCADE,
    ADD COLUMN actor_security_version bigint,
    ADD COLUMN recent_auth_purpose text,
    ADD COLUMN recent_auth_resource_id uuid,
    ADD CONSTRAINT oidc_authorization_flows_purpose_check CHECK (
        purpose IN ('login', 'link', 'reauthenticate')
    ),
    ADD CONSTRAINT oidc_authorization_flows_security_context CHECK (
        (
            purpose = 'login'
            AND actor_user_id IS NULL
            AND actor_session_id IS NULL
            AND actor_security_version IS NULL
            AND recent_auth_purpose IS NULL
            AND recent_auth_resource_id IS NULL
            AND client_digest IS NOT NULL
        ) OR (
            purpose = 'link'
            AND actor_user_id IS NOT NULL
            AND actor_session_id IS NOT NULL
            AND actor_security_version > 0
            AND recent_auth_purpose IS NULL
            AND recent_auth_resource_id IS NULL
            AND client_digest IS NULL
        ) OR (
            purpose = 'reauthenticate'
            AND actor_user_id IS NOT NULL
            AND actor_session_id IS NOT NULL
            AND actor_security_version > 0
            AND recent_auth_purpose IN (
                'password_enrollment', 'oidc_link', 'oidc_unlink'
            )
            AND (
                (recent_auth_purpose = 'oidc_unlink' AND recent_auth_resource_id IS NOT NULL)
                OR (recent_auth_purpose <> 'oidc_unlink' AND recent_auth_resource_id IS NULL)
            )
            AND client_digest IS NULL
        )
    );

CREATE INDEX oidc_authorization_flows_actor_session_idx
    ON oidc_authorization_flows(actor_session_id)
    WHERE actor_session_id IS NOT NULL;
