ALTER TABLE olp.projects ADD COLUMN attribution_policy jsonb CHECK (attribution_policy IS NULL OR jsonb_typeof(attribution_policy)='object');
ALTER TABLE olp.code_attempts ADD COLUMN attribution jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(attribution)='object');
