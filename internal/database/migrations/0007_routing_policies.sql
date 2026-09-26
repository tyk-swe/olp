CREATE TABLE olp.routing_policies (
    scope text NOT NULL CHECK (scope IN ('installation','route-draft','api-key')),
    scope_id uuid NOT NULL,
    policy jsonb NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL REFERENCES olp.users,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, scope_id)
);
ALTER TABLE olp.route_revisions ADD COLUMN routing_policy jsonb;
CREATE INDEX attempts_routing_measurements ON olp.attempts (completed_at DESC)
    WHERE error_class IS NULL AND status_code BETWEEN 200 AND 299;
