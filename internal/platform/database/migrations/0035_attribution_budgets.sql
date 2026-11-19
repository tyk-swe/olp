ALTER TABLE olp.projects ADD COLUMN attribution_budgets jsonb NOT NULL DEFAULT '{}'::jsonb
 CHECK (jsonb_typeof(attribution_budgets)='object');
ALTER TABLE olp.aggregate_budget_accounts
 DROP CONSTRAINT aggregate_budget_accounts_project_id_key,
 ADD COLUMN attribution_key text CHECK (attribution_key ~ '^[A-Za-z][A-Za-z0-9_.-]{0,31}$'),
 ADD COLUMN attribution_value text CHECK (attribution_value ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
 ADD CONSTRAINT aggregate_attribution_owner CHECK (
  (attribution_key IS NULL)=(attribution_value IS NULL) AND
  (attribution_key IS NULL OR project_id IS NOT NULL)),
 ADD CONSTRAINT aggregate_project_attribution UNIQUE (project_id,attribution_key,attribution_value);
CREATE UNIQUE INDEX aggregate_project_budget ON olp.aggregate_budget_accounts(project_id)
 WHERE attribution_key IS NULL;
