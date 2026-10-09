-- SCIM owns only identities explicitly enrolled through its provisioning source.
-- Local takeover is respected by role reconciliation and inherited memberships.
CREATE TABLE olp.scim_users (
 user_id uuid PRIMARY KEY REFERENCES olp.users(id),
 document jsonb NOT NULL CHECK(jsonb_typeof(document)='object'),
 deleted_at timestamptz
);
CREATE UNIQUE INDEX scim_users_external ON olp.scim_users ((document->>'externalId')) WHERE document->>'externalId' IS NOT NULL;
CREATE TABLE olp.scim_groups (
 id uuid PRIMARY KEY, document jsonb NOT NULL CHECK(jsonb_typeof(document)='object'),
 etag uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX scim_groups_name ON olp.scim_groups(lower(document->>'displayName'));
CREATE UNIQUE INDEX scim_groups_external ON olp.scim_groups((document->>'externalId')) WHERE document->>'externalId' IS NOT NULL;
CREATE TABLE olp.scim_group_members (
 group_id uuid NOT NULL REFERENCES olp.scim_groups(id) ON DELETE CASCADE,
 user_id uuid NOT NULL REFERENCES olp.scim_users(user_id),PRIMARY KEY(group_id,user_id)
);
CREATE INDEX scim_group_members_user ON olp.scim_group_members(user_id);
CREATE TABLE olp.scim_group_projects (
 group_id uuid NOT NULL REFERENCES olp.scim_groups(id) ON DELETE CASCADE,
 project_id uuid NOT NULL REFERENCES olp.projects(id) ON DELETE CASCADE,
 role text NOT NULL CHECK(role IN ('manager','viewer')),PRIMARY KEY(group_id,project_id)
);
CREATE OR REPLACE VIEW olp.effective_project_members AS
 SELECT project_id,user_id,CASE WHEN bool_or(role='manager') THEN 'manager' ELSE 'viewer' END AS role
 FROM (
  SELECT project_id,user_id,role FROM olp.project_members
  UNION ALL
  SELECT p.id,m.user_id,m.role FROM olp.projects p JOIN olp.organization_members m ON m.organization_id=p.organization_id
  UNION ALL
  SELECT p.project_id,m.user_id,p.role FROM olp.scim_group_projects p JOIN olp.scim_group_members m ON m.group_id=p.group_id
  JOIN olp.scim_users su ON su.user_id=m.user_id JOIN olp.users u ON u.id=m.user_id
  WHERE su.deleted_at IS NULL AND u.role_management='provisioned'
 ) memberships GROUP BY project_id,user_id;

-- Computed member displays and membership removal change Group representations.
-- Keep their ETags accurate for concurrent directory clients and console edits.
CREATE FUNCTION olp.scim_touch_member_groups() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE olp.scim_groups SET etag=uuidv7(),updated_at=now()
 WHERE id IN (SELECT group_id FROM olp.scim_group_members WHERE user_id=NEW.id);
 RETURN NULL;
END $$;
CREATE TRIGGER scim_user_display_changed AFTER UPDATE OF display_name,role_management ON olp.users
 FOR EACH ROW WHEN (OLD.display_name IS DISTINCT FROM NEW.display_name OR OLD.role_management IS DISTINCT FROM NEW.role_management)
 EXECUTE FUNCTION olp.scim_touch_member_groups();
CREATE FUNCTION olp.scim_touch_removed_member_groups() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE olp.scim_groups SET etag=uuidv7(),updated_at=now()
 WHERE id IN (SELECT group_id FROM removed_scim_members);
 RETURN NULL;
END $$;
CREATE TRIGGER scim_members_removed AFTER DELETE ON olp.scim_group_members
 REFERENCING OLD TABLE AS removed_scim_members FOR EACH STATEMENT
 EXECUTE FUNCTION olp.scim_touch_removed_member_groups();
