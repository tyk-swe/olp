ALTER TABLE olp.projects ADD COLUMN limit_templates jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(limit_templates)='object');
ALTER TABLE olp.budget_groups
 ADD COLUMN limit_template text,
 ADD COLUMN requests_per_minute bigint CHECK (requests_per_minute BETWEEN 1 AND 2147483647),
 ADD COLUMN tokens_per_minute bigint CHECK (tokens_per_minute BETWEEN 1 AND 9007199254740991),
 ADD COLUMN max_concurrency bigint CHECK (max_concurrency BETWEEN 1 AND 2147483647),
 DROP CONSTRAINT budget_groups_check,
 ADD CHECK (limit_template IS NOT NULL OR daily_cost_limit IS NOT NULL OR monthly_cost_limit IS NOT NULL OR requests_per_minute IS NOT NULL OR tokens_per_minute IS NOT NULL OR max_concurrency IS NOT NULL);

-- Mirrors access.intersectLimits for reporting, reconciliation and notifications.
-- Authorization compiles the same policy once per authority refresh.
CREATE FUNCTION olp.effective_limits(local_limits jsonb, template_limits jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT jsonb_build_object(
 'requests_per_minute',LEAST((local_limits->>'requests_per_minute')::bigint,(template_limits->>'requests_per_minute')::bigint),
 'tokens_per_minute',LEAST((local_limits->>'tokens_per_minute')::bigint,(template_limits->>'tokens_per_minute')::bigint),
 'max_concurrency',LEAST((local_limits->>'max_concurrency')::bigint,(template_limits->>'max_concurrency')::bigint),
 'daily_cost_limit',LEAST((local_limits->>'daily_cost_limit')::numeric,(template_limits->>'daily_cost_limit')::numeric)::text,
 'monthly_cost_limit',LEAST((local_limits->>'monthly_cost_limit')::numeric,(template_limits->>'monthly_cost_limit')::numeric)::text);
$$;
CREATE VIEW olp.api_keys_with_limits AS SELECT k.*,olp.effective_limits(k.policy,p.limit_templates->(k.policy->>'limit_template')) AS effective_limits FROM olp.api_keys k LEFT JOIN olp.projects p ON p.id=k.project_id;
CREATE VIEW olp.budget_groups_with_limits AS SELECT g.*,olp.effective_limits(to_jsonb(g),p.limit_templates->g.limit_template) AS effective_limits FROM olp.budget_groups g LEFT JOIN olp.projects p ON p.id=g.project_id;
