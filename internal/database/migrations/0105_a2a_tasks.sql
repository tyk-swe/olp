CREATE TABLE olp.a2a_tasks (
 id uuid PRIMARY KEY,
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 project_id uuid NOT NULL REFERENCES olp.projects,
 publication_id uuid NOT NULL REFERENCES olp.agent_publications,
 revision_id uuid NOT NULL REFERENCES olp.agent_publication_revisions,
 dispatch_id uuid NOT NULL REFERENCES olp.execution_debits,
 upstream_id text NOT NULL CHECK(length(upstream_id) BETWEEN 1 AND 1024),
 upstream_context_id text NOT NULL DEFAULT '' CHECK(length(upstream_context_id)<=1024),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(api_key_id,project_id,publication_id,upstream_id)
);
