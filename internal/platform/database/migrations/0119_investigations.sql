CREATE TABLE olp.investigations (
 id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects(id) ON DELETE CASCADE,
 filters jsonb NOT NULL,
 evidence jsonb NOT NULL,
 summary text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX investigations_project_id ON olp.investigations(project_id, id DESC);
