-- M6.2 budget basis: every consumer budget chooses whether it enforces
-- provider cost ("cost", the default) or consumer charge ("charge"). Each
-- cost window therefore carries both totals: accrued/unpriced_attempts keep
-- their meaning, charged/uncharged_attempts sum the consumer charge and the
-- billable attempts whose charge could not be computed.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['api_key_cost_windows','budget_group_cost_windows','end_user_cost_windows','aggregate_cost_windows'] LOOP
    EXECUTE format('ALTER TABLE olp.%I
      ADD COLUMN charged numeric(28,12) NOT NULL DEFAULT 0 CHECK (charged >= 0),
      ADD COLUMN uncharged_attempts bigint NOT NULL DEFAULT 0 CHECK (uncharged_attempts >= 0)', t);
    EXECUTE format('ALTER TABLE olp.%I ADD CONSTRAINT %I CHECK (window_kind = ''month'' OR uncharged_attempts = 0)', t, t || '_uncharged_scope_check');
  END LOOP;
END $$;

-- A budget group's basis lives beside its limits.
ALTER TABLE olp.budget_groups
    ADD COLUMN cost_basis text NOT NULL DEFAULT 'cost'
        CHECK (cost_basis IN ('cost','charge'));

-- A template supplies the basis when the holder does not set one.
CREATE OR REPLACE FUNCTION olp.effective_limits(local_limits jsonb, template_limits jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT jsonb_build_object(
 'requests_per_minute',LEAST((local_limits->>'requests_per_minute')::bigint,(template_limits->>'requests_per_minute')::bigint),
 'tokens_per_minute',LEAST((local_limits->>'tokens_per_minute')::bigint,(template_limits->>'tokens_per_minute')::bigint),
 'max_concurrency',LEAST((local_limits->>'max_concurrency')::bigint,(template_limits->>'max_concurrency')::bigint),
 'daily_cost_limit',LEAST((local_limits->>'daily_cost_limit')::numeric,(template_limits->>'daily_cost_limit')::numeric)::text,
 'weekly_cost_limit',LEAST((local_limits->>'weekly_cost_limit')::numeric,(template_limits->>'weekly_cost_limit')::numeric)::text,
 'monthly_cost_limit',LEAST((local_limits->>'monthly_cost_limit')::numeric,(template_limits->>'monthly_cost_limit')::numeric)::text,
 'cost_basis',COALESCE(local_limits->>'cost_basis',template_limits->>'cost_basis','cost'));
$$;

-- The view lists its columns explicitly, so the new column is not in it
-- until it is recreated.
CREATE OR REPLACE VIEW olp.budget_groups_with_limits AS
SELECT g.id,g.name,g.project_id,g.daily_cost_limit,g.monthly_cost_limit,g.etag,g.created_by,g.created_at,g.updated_at,
       g.limit_template,g.requests_per_minute,g.tokens_per_minute,g.max_concurrency,g.weekly_cost_limit,
       olp.effective_limits(to_jsonb(g),p.limit_templates->g.limit_template) AS effective_limits,g.retired_at,g.cost_basis
FROM olp.budget_groups g LEFT JOIN olp.projects p ON p.id=g.project_id
WHERE g.retired_at IS NULL;
