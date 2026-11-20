ALTER TABLE olp.agent_delegations DROP CONSTRAINT agent_delegations_allowed_routes_check;
ALTER TABLE olp.agent_delegations ADD COLUMN scopes text[] NOT NULL DEFAULT ARRAY['inference'];
ALTER TABLE olp.agent_delegations ADD COLUMN allowed_toolsets text[] NOT NULL DEFAULT ARRAY[]::text[];
ALTER TABLE olp.agent_delegations ADD CONSTRAINT agent_delegations_resources_check
    CHECK (cardinality(allowed_routes) + cardinality(allowed_toolsets) > 0);
