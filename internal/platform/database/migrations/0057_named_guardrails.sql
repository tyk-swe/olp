CREATE TABLE olp.guardrails (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects(id),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    type text NOT NULL CHECK (type = 'builtin.regex'),
    latest_revision_id uuid NOT NULL,
    etag uuid NOT NULL,
    retired_at timestamptz,
    created_by uuid NOT NULL REFERENCES olp.users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX guardrails_project_name ON olp.guardrails(project_id, lower(name)) WHERE retired_at IS NULL;

CREATE TABLE olp.guardrail_revisions (
    id uuid PRIMARY KEY,
    guardrail_id uuid NOT NULL REFERENCES olp.guardrails(id),
    revision bigint NOT NULL CHECK (revision > 0),
    policy jsonb NOT NULL CHECK (jsonb_typeof(policy) = 'object'),
    created_by uuid NOT NULL REFERENCES olp.users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (guardrail_id, revision),
    UNIQUE (id, guardrail_id)
);
ALTER TABLE olp.guardrails ADD CONSTRAINT guardrail_latest_revision_owner
    FOREIGN KEY (latest_revision_id, id) REFERENCES olp.guardrail_revisions(id, guardrail_id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION olp.guardrail_revision_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'guardrail revisions are immutable' USING ERRCODE = '55000';
END;
$$;
CREATE TRIGGER guardrail_revision_immutable BEFORE UPDATE OR DELETE ON olp.guardrail_revisions
    FOR EACH ROW EXECUTE FUNCTION olp.guardrail_revision_immutable();
