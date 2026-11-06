CREATE TABLE olp.code_decisions (
    id uuid PRIMARY KEY,
    attempt_id uuid REFERENCES olp.code_attempts,
    project_id uuid NOT NULL REFERENCES olp.projects,
    route_id uuid NOT NULL REFERENCES olp.code_routes,
    route_revision_id uuid NOT NULL REFERENCES olp.code_route_revisions,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    binding_id uuid NOT NULL REFERENCES olp.code_bindings,
    root_id uuid NOT NULL REFERENCES olp.code_bindings,
    chosen_account_id uuid NOT NULL REFERENCES olp.code_accounts,
    reason text NOT NULL CHECK (reason IN ('chosen','pin','connection')),
    band text CHECK (band IN ('healthy','unknown','low','exhausted')),
    evidence text CHECK (evidence IN ('passive','active')),
    evidence_observed_at timestamptz,
    skipped jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(skipped)='array'),
    recovery_envelope text CHECK (recovery_envelope ~ '^[a-z0-9][a-z0-9_]{0,99}$'),
    recovery_ordinal integer CHECK (recovery_ordinal>=1),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((evidence IS NULL) = (evidence_observed_at IS NULL))
);
CREATE UNIQUE INDEX code_decisions_attempt ON olp.code_decisions(attempt_id) WHERE attempt_id IS NOT NULL;
CREATE INDEX code_decisions_project ON olp.code_decisions(project_id,id);
