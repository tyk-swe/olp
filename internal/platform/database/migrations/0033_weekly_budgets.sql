-- Weekly snapshots are unavailable until rebuilt from complete usage history.
-- Incremental delivery alone cannot establish a newly introduced window.
DO $$
DECLARE table_name text; constraint_name text;
BEGIN
 FOREACH table_name IN ARRAY ARRAY['api_key_cost_windows','budget_group_cost_windows','end_user_cost_windows','aggregate_cost_windows','supply_cost_windows'] LOOP
  FOR constraint_name IN SELECT c.conname FROM pg_constraint c
   WHERE c.conrelid=('olp.'||table_name)::regclass AND c.contype='c'
   AND pg_get_constraintdef(c.oid) LIKE '%window_kind%'
   AND pg_get_constraintdef(c.oid) LIKE '%day%'
   AND pg_get_constraintdef(c.oid) LIKE '%month%'
   AND pg_get_constraintdef(c.oid) NOT LIKE '%unpriced%'
  LOOP EXECUTE format('ALTER TABLE olp.%I DROP CONSTRAINT %I',table_name,constraint_name); END LOOP;
  EXECUTE format('ALTER TABLE olp.%I ADD CHECK(window_kind IN (''day'',''week'',''month'')), ADD COLUMN weekly_complete boolean NOT NULL DEFAULT false',table_name);
 END LOOP;
END $$;

DROP VIEW olp.api_keys_with_limits;
DROP VIEW olp.budget_groups_with_limits;
ALTER TABLE olp.budget_groups ADD COLUMN weekly_cost_limit numeric(24,12) CHECK(weekly_cost_limit>0);
DO $$ DECLARE n text; BEGIN
 FOR n IN SELECT conname FROM pg_constraint WHERE conrelid='olp.budget_groups'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%daily_cost_limit%' AND pg_get_constraintdef(oid) LIKE '%max_concurrency%' LOOP
 EXECUTE format('ALTER TABLE olp.budget_groups DROP CONSTRAINT %I',n);
 END LOOP;
END $$;
ALTER TABLE olp.budget_groups ADD CHECK(limit_template IS NOT NULL OR daily_cost_limit IS NOT NULL OR weekly_cost_limit IS NOT NULL OR monthly_cost_limit IS NOT NULL OR requests_per_minute IS NOT NULL OR tokens_per_minute IS NOT NULL OR max_concurrency IS NOT NULL);
CREATE OR REPLACE FUNCTION olp.effective_limits(local_limits jsonb, template_limits jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT jsonb_build_object(
 'requests_per_minute',LEAST((local_limits->>'requests_per_minute')::bigint,(template_limits->>'requests_per_minute')::bigint),
 'tokens_per_minute',LEAST((local_limits->>'tokens_per_minute')::bigint,(template_limits->>'tokens_per_minute')::bigint),
 'max_concurrency',LEAST((local_limits->>'max_concurrency')::bigint,(template_limits->>'max_concurrency')::bigint),
 'daily_cost_limit',LEAST((local_limits->>'daily_cost_limit')::numeric,(template_limits->>'daily_cost_limit')::numeric)::text,
 'weekly_cost_limit',LEAST((local_limits->>'weekly_cost_limit')::numeric,(template_limits->>'weekly_cost_limit')::numeric)::text,
 'monthly_cost_limit',LEAST((local_limits->>'monthly_cost_limit')::numeric,(template_limits->>'monthly_cost_limit')::numeric)::text);
$$;
CREATE VIEW olp.api_keys_with_limits AS SELECT k.*,olp.effective_limits(k.policy,p.limit_templates->(k.policy->>'limit_template')) AS effective_limits FROM olp.api_keys k LEFT JOIN olp.projects p ON p.id=k.project_id;
CREATE VIEW olp.budget_groups_with_limits AS SELECT g.*,olp.effective_limits(to_jsonb(g),p.limit_templates->g.limit_template) AS effective_limits FROM olp.budget_groups g LEFT JOIN olp.projects p ON p.id=g.project_id;

-- Retain each rule/delivery's remaining evidence constraints while extending
-- the shared calendar-period enumeration.
DO $$ DECLARE r record; BEGIN
 FOR r IN SELECT conrelid::regclass AS rel,conname,pg_get_constraintdef(oid) AS definition FROM pg_constraint
 WHERE conrelid IN ('olp.notification_rules'::regclass,'olp.notification_deliveries'::regclass) AND contype='c'
 AND pg_get_constraintdef(oid) LIKE '%window_kind%' AND pg_get_constraintdef(oid) LIKE '%day%' AND pg_get_constraintdef(oid) LIKE '%month%'
 LOOP
 EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',r.rel,r.conname);
 EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s',r.rel,r.conname,replace(r.definition,'''month''::text','''week''::text, ''month''::text'));
 END LOOP;
END $$;
