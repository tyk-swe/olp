ALTER TABLE olp.api_keys ADD COLUMN paused_at timestamptz;
ALTER TABLE olp.api_keys ADD COLUMN paused_authority_sequence bigint;
ALTER TABLE olp.runtime_install_status ADD COLUMN authority_sequence bigint NOT NULL DEFAULT 0;
DROP VIEW olp.api_keys_with_limits;
CREATE VIEW olp.api_keys_with_limits AS SELECT k.*,olp.effective_limits(k.policy,p.limit_templates->(k.policy->>'limit_template')) AS effective_limits FROM olp.api_keys k LEFT JOIN olp.projects p ON p.id=k.project_id;
