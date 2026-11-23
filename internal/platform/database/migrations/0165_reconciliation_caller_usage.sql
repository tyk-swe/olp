ALTER TABLE olp.attempt_usage_hourly
 ADD COLUMN caller_credential boolean NOT NULL DEFAULT false,
 ADD COLUMN reconcil_caller_attempts bigint,
 DROP CONSTRAINT attempt_usage_hourly_dimensions_key,
 ADD CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
 (bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,budget_group_id,
 attribution,model_family,estimate_provenance,end_user_digest,budget_bucket,budget_exempt,
 commitment_amount,commitment_period,agent_principal_id,delegation_pair_digest,caller_credential);
