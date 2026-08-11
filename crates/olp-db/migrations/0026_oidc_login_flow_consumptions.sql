-- Stateless OIDC login starts keep their encrypted PKCE material in the
-- browser, but a valid callback must still be globally single-use. The opaque
-- flow UUID is authenticated inside that cookie and is recorded only when a
-- callback first presents the matching state. This avoids unauthenticated
-- database writes at login start while making callback consumption atomic
-- across every control-plane replica.
CREATE TABLE oidc_login_flow_consumptions (
    flow_id uuid PRIMARY KEY,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > consumed_at)
);

CREATE INDEX oidc_login_flow_consumptions_expires_at_idx
    ON oidc_login_flow_consumptions (expires_at);
