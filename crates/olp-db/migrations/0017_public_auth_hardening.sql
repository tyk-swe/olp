-- Bound unauthenticated password work across every control-plane replica. Only
-- opaque SHA-256 digests are retained; submitted email addresses and invitation
-- tokens never enter this table.
CREATE TABLE public_auth_rate_limits (
    action text NOT NULL CHECK (action IN ('local_login', 'invitation_acceptance')),
    scope text NOT NULL CHECK (scope IN ('global', 'client')),
    key_digest bytea NOT NULL CHECK (octet_length(key_digest) = 32),
    window_started_at timestamptz NOT NULL,
    attempts integer NOT NULL CHECK (attempts > 0),
    PRIMARY KEY (action, scope, key_digest)
);

CREATE INDEX public_auth_rate_limits_window_idx
    ON public_auth_rate_limits (window_started_at);

-- A stable, opaque browser cookie identifies ordinary OIDC clients. The
-- global rolling admission window remains authoritative when an attacker
-- rotates or refuses that cookie.
-- Outstanding pre-upgrade flows have no client binding and are deliberately
-- invalidated; callers can safely start authorization again.
DELETE FROM oidc_authorization_flows;

ALTER TABLE oidc_authorization_flows
    ADD COLUMN client_digest bytea,
    ADD CONSTRAINT oidc_authorization_flows_client_digest_length CHECK (
        client_digest IS NULL OR octet_length(client_digest) = 32
    ),
    ADD CONSTRAINT oidc_authorization_flows_login_client CHECK (
        (purpose = 'login' AND client_digest IS NOT NULL)
        OR (purpose = 'link' AND client_digest IS NULL)
    );

CREATE INDEX oidc_authorization_flows_login_rate_idx
    ON oidc_authorization_flows (purpose, created_at);

CREATE INDEX oidc_authorization_flows_client_rate_idx
    ON oidc_authorization_flows (client_digest, created_at)
    WHERE purpose = 'login';
