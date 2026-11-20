ALTER TABLE olp.agent_delegations DROP CONSTRAINT agent_delegations_resources_check;
ALTER TABLE olp.agent_delegations ADD COLUMN allowed_agents text[] NOT NULL DEFAULT ARRAY[]::text[];
ALTER TABLE olp.agent_delegations ADD CONSTRAINT agent_delegations_resources_check
    CHECK (cardinality(allowed_routes) + cardinality(allowed_toolsets) + cardinality(allowed_agents) > 0);

ALTER TABLE olp.a2a_tasks
 ADD COLUMN delegation_pair_digest text NOT NULL DEFAULT '',
 ADD COLUMN delegation_agent_id uuid REFERENCES olp.api_keys,
 ADD COLUMN delegation_member_id uuid REFERENCES olp.users,
 ADD COLUMN delegation_token_digest text NOT NULL DEFAULT '';

ALTER TABLE olp.a2a_tasks DROP CONSTRAINT a2a_tasks_api_key_id_project_id_publication_id_upstream_id_key;
CREATE UNIQUE INDEX a2a_tasks_key_owner ON olp.a2a_tasks(api_key_id,project_id,publication_id,upstream_id) WHERE delegation_pair_digest='';
CREATE UNIQUE INDEX a2a_tasks_delegated_owner ON olp.a2a_tasks(project_id,publication_id,delegation_pair_digest,upstream_id) WHERE delegation_pair_digest<>'';
