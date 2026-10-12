-- Original accounting identities outlive current budget policy. A key may
-- leave a group while its first response/event is still in flight.
ALTER TABLE olp.budget_groups ADD COLUMN retired_at timestamptz;
ALTER TABLE olp.budget_groups ALTER COLUMN created_by DROP NOT NULL;
DO $$ DECLARE n text; BEGIN
 FOR n IN SELECT conname FROM pg_constraint
 WHERE conrelid='olp.budget_groups'::regclass AND contype='c'
 AND pg_get_constraintdef(oid) LIKE '%daily_cost_limit%'
 AND pg_get_constraintdef(oid) LIKE '%max_concurrency%' LOOP
  EXECUTE format('ALTER TABLE olp.budget_groups DROP CONSTRAINT %I',n);
 END LOOP;
END $$;
ALTER TABLE olp.budget_groups ADD CONSTRAINT budget_group_policy_lifetime CHECK (
 (retired_at IS NULL AND created_by IS NOT NULL AND
  (limit_template IS NOT NULL OR daily_cost_limit IS NOT NULL OR weekly_cost_limit IS NOT NULL OR
   monthly_cost_limit IS NOT NULL OR requests_per_minute IS NOT NULL OR tokens_per_minute IS NOT NULL OR max_concurrency IS NOT NULL))
 OR
 (retired_at IS NOT NULL AND limit_template IS NULL AND daily_cost_limit IS NULL AND weekly_cost_limit IS NULL AND
  monthly_cost_limit IS NULL AND requests_per_minute IS NULL AND tokens_per_minute IS NULL AND max_concurrency IS NULL)
);
DROP INDEX olp.budget_groups_name_scope;
CREATE UNIQUE INDEX budget_groups_name_scope ON olp.budget_groups
 (lower(name),COALESCE(project_id,'00000000-0000-0000-0000-000000000000'::uuid)) WHERE retired_at IS NULL;

-- Old pending templates can be the only surviving reference after a physical
-- deletion. Restore only their opaque identity, never guessed policy, project
-- or creator provenance. Such shells cannot be viewed, edited or reassigned.
INSERT INTO olp.budget_groups(id,name,etag,created_by,created_at,updated_at,retired_at)
SELECT original.id,'retired:'||original.id::text,original.id,NULL,
       original.first_reference,now(),now()
FROM (
 SELECT (metadata->'pending_usage'->>'budget_group_id')::uuid AS id,min(created_at) AS first_reference
 FROM olp.provider_resources
 WHERE olp.resource_owns_pending_usage(kind)
   AND NULLIF(metadata->'pending_usage'->>'budget_group_id','') IS NOT NULL
 GROUP BY (metadata->'pending_usage'->>'budget_group_id')::uuid
) original
WHERE NOT EXISTS(SELECT 1 FROM olp.budget_groups g WHERE g.id=original.id);
ALTER TABLE olp.provider_resource_pending_usage ADD CONSTRAINT pending_usage_original_group
 FOREIGN KEY(budget_group_id) REFERENCES olp.budget_groups;

-- Keep existing view columns in order; append the new internal lifetime fact.
CREATE OR REPLACE VIEW olp.budget_groups_with_limits AS
SELECT g.id,g.name,g.project_id,g.daily_cost_limit,g.monthly_cost_limit,g.etag,g.created_by,g.created_at,g.updated_at,
       g.limit_template,g.requests_per_minute,g.tokens_per_minute,g.max_concurrency,g.weekly_cost_limit,
       olp.effective_limits(to_jsonb(g),p.limit_templates->g.limit_template) AS effective_limits,g.retired_at
FROM olp.budget_groups g LEFT JOIN olp.projects p ON p.id=g.project_id
WHERE g.retired_at IS NULL;
