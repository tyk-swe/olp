-- Named allowlist sets stay in their project and refresh with key authority.
ALTER TABLE olp.projects ADD COLUMN route_groups jsonb NOT NULL DEFAULT '{}'
 CHECK (jsonb_typeof(route_groups)='object');
