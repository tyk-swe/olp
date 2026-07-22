-- Outstanding link redirects from earlier releases are bound only to a user,
-- not the exact session that initiated them. Invalidate those short-lived rows
-- and make future link flow consumption conditional on the initiating session.
DELETE FROM oidc_authorization_flows;

ALTER TABLE oidc_authorization_flows
    ADD COLUMN actor_session_id uuid REFERENCES sessions(id) ON DELETE CASCADE,
    ADD CONSTRAINT oidc_authorization_flows_actor_session CHECK (
        (purpose = 'login' AND actor_session_id IS NULL)
        OR (purpose = 'link' AND actor_session_id IS NOT NULL)
    );

CREATE INDEX oidc_authorization_flows_actor_session_idx
    ON oidc_authorization_flows (actor_session_id)
    WHERE actor_session_id IS NOT NULL;
