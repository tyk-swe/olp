CREATE TABLE olp_go.budget_groups (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    project_id uuid REFERENCES olp_go.projects,
    daily_cost_limit numeric(24,12),
    monthly_cost_limit numeric(24,12),
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp_go.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (daily_cost_limit IS NOT NULL OR monthly_cost_limit IS NOT NULL),
    CHECK (daily_cost_limit IS NULL OR daily_cost_limit > 0),
    CHECK (monthly_cost_limit IS NULL OR monthly_cost_limit > 0)
);
CREATE UNIQUE INDEX budget_groups_name_scope ON olp_go.budget_groups
    (lower(name), COALESCE(project_id,'00000000-0000-0000-0000-000000000000'::uuid));
ALTER TABLE olp_go.api_keys ADD COLUMN budget_group_id uuid REFERENCES olp_go.budget_groups;
CREATE TABLE olp_go.budget_group_cost_windows (
    budget_group_id uuid NOT NULL REFERENCES olp_go.budget_groups,
    window_kind text NOT NULL CHECK (window_kind IN ('day','month')),
    window_id bigint NOT NULL CHECK (window_id >= 0),
    accrued numeric(28,12) NOT NULL CHECK (accrued >= 0),
    unpriced_attempts bigint NOT NULL CHECK (unpriced_attempts >= 0),
    CHECK (window_kind='month' OR unpriced_attempts=0),
    PRIMARY KEY(budget_group_id,window_kind,window_id)
);
ALTER TABLE olp_go.requests ADD COLUMN budget_group_id uuid REFERENCES olp_go.budget_groups;
ALTER TABLE olp_go.attempt_usage_facts ADD COLUMN budget_group_id uuid REFERENCES olp_go.budget_groups;
ALTER TABLE olp_go.attempt_usage_hourly ADD COLUMN budget_group_id uuid REFERENCES olp_go.budget_groups;
ALTER TABLE olp_go.attempt_usage_hourly DROP CONSTRAINT attempt_usage_hourly_dimensions_key;
ALTER TABLE olp_go.attempt_usage_hourly ADD CONSTRAINT attempt_usage_hourly_dimensions_key
    UNIQUE NULLS NOT DISTINCT (bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,budget_group_id);
