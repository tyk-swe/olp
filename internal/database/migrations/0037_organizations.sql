CREATE TABLE olp.organizations (
 id uuid PRIMARY KEY, name text NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 etag uuid NOT NULL, created_by uuid NOT NULL REFERENCES olp.users(id),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 budget_policy jsonb CHECK(budget_policy IS NULL OR jsonb_typeof(budget_policy)='object')
);
CREATE UNIQUE INDEX organizations_name ON olp.organizations(lower(name));
CREATE TABLE olp.organization_members (
 organization_id uuid NOT NULL REFERENCES olp.organizations(id) ON DELETE CASCADE,
 user_id uuid NOT NULL REFERENCES olp.users(id) ON DELETE CASCADE,
 role text NOT NULL CHECK(role IN ('manager','viewer')),
 added_by uuid NOT NULL REFERENCES olp.users(id), created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(organization_id,user_id)
);
ALTER TABLE olp.projects ADD COLUMN organization_id uuid REFERENCES olp.organizations(id);
CREATE INDEX projects_organization ON olp.projects(organization_id) WHERE organization_id IS NOT NULL;
CREATE VIEW olp.effective_project_members AS
 SELECT project_id,user_id,CASE WHEN bool_or(role='manager') THEN 'manager' ELSE 'viewer' END AS role
 FROM (
  SELECT project_id,user_id,role FROM olp.project_members
  UNION ALL
  SELECT p.id,m.user_id,m.role FROM olp.projects p JOIN olp.organization_members m ON m.organization_id=p.organization_id
 ) memberships GROUP BY project_id,user_id;
ALTER TABLE olp.aggregate_budget_accounts
 ADD COLUMN organization_id uuid UNIQUE REFERENCES olp.organizations(id) ON DELETE CASCADE,
 DROP CONSTRAINT aggregate_budget_owner,
 ADD CONSTRAINT aggregate_budget_owner CHECK (
  num_nonnulls(installation_id,organization_id,project_id,api_key_id)=1 AND (api_key_id IS NULL)=(route_slug IS NULL));
ALTER TABLE olp.budget_increases ADD COLUMN organization_id uuid REFERENCES olp.organizations(id) ON DELETE CASCADE;
CREATE INDEX budget_increases_organization ON olp.budget_increases(organization_id,id DESC) WHERE organization_id IS NOT NULL;
