ALTER TABLE olp.attempt_usage_hourly
 ADD COLUMN agent_principal_id uuid,
 ADD COLUMN delegation_pair_digest text NOT NULL DEFAULT '';

ALTER TABLE olp.attempt_usage_hourly DROP CONSTRAINT attempt_usage_hourly_dimensions_key,
 ADD CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
 (bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,budget_group_id,
 attribution,model_family,estimate_provenance,end_user_digest,budget_bucket,budget_exempt,
 commitment_amount,commitment_period,agent_principal_id,delegation_pair_digest);

CREATE INDEX attempt_usage_hourly_agent_budget ON olp.attempt_usage_hourly (agent_principal_id, budget_bucket) WHERE agent_principal_id IS NOT NULL;
CREATE INDEX attempt_usage_hourly_pair_budget ON olp.attempt_usage_hourly (delegation_pair_digest, budget_bucket) WHERE delegation_pair_digest <> '';
