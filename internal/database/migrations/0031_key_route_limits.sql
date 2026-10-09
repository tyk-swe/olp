ALTER TABLE olp.aggregate_budget_accounts
 ADD COLUMN api_key_id uuid REFERENCES olp.api_keys(id) ON DELETE CASCADE,
 ADD COLUMN route_slug text CHECK (route_slug ~ '^[a-z0-9][a-z0-9._-]{0,99}$'),
 DROP CONSTRAINT aggregate_budget_accounts_check,
 ADD CONSTRAINT aggregate_budget_owner CHECK (
  num_nonnulls(installation_id,project_id,api_key_id)=1 AND
  (api_key_id IS NULL)=(route_slug IS NULL)),
 ADD CONSTRAINT aggregate_key_route UNIQUE (api_key_id,route_slug);
