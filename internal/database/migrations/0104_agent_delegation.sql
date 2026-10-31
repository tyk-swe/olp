CREATE TABLE olp.agent_delegations (
    token_digest bytea PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects(id),
    api_key_id uuid NOT NULL REFERENCES olp.api_keys(id),
    agent_id uuid NOT NULL REFERENCES olp.api_keys(id),
    issuer_revision uuid NOT NULL,
    allowed_routes text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    revoked_at timestamptz,
    CHECK (cardinality(allowed_routes) > 0)
);

ALTER TABLE olp.attempt_usage_facts ADD COLUMN agent_principal_id uuid REFERENCES olp.api_keys(id);
ALTER TABLE olp.attempt_usage_facts ADD COLUMN delegation_pair_digest text;
ALTER TABLE olp.attempt_usage_facts ADD CONSTRAINT attempt_delegation_pair CHECK (
    (agent_principal_id IS NULL AND delegation_pair_digest IS NULL) OR
    (agent_principal_id IS NOT NULL AND delegation_pair_digest IS NOT NULL AND delegation_pair_digest ~ '^[0-9a-f]{64}$')
);
