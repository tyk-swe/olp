CREATE TABLE olp_go.projects (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp_go.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX projects_name ON olp_go.projects (lower(name));
CREATE TABLE olp_go.project_members (
    project_id uuid NOT NULL REFERENCES olp_go.projects ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES olp_go.users ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('manager','viewer')),
    added_by uuid NOT NULL REFERENCES olp_go.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(project_id,user_id)
);
ALTER TABLE olp_go.users ADD COLUMN access_scope text NOT NULL DEFAULT 'global' CHECK (access_scope IN ('global','assigned'));
ALTER TABLE olp_go.providers ADD COLUMN project_id uuid REFERENCES olp_go.projects;
ALTER TABLE olp_go.route_drafts ADD COLUMN project_id uuid REFERENCES olp_go.projects;
ALTER TABLE olp_go.routes ADD COLUMN project_id uuid REFERENCES olp_go.projects;
ALTER TABLE olp_go.api_keys ADD COLUMN project_id uuid REFERENCES olp_go.projects;
ALTER TABLE olp_go.management_tokens
    ADD COLUMN all_projects boolean NOT NULL DEFAULT true,
    ADD COLUMN project_ids jsonb NOT NULL DEFAULT '[]'::jsonb;
