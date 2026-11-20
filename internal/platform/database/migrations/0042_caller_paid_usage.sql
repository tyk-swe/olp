-- Keep priced usage observable while preserving the original budget treatment
-- through delayed accounting, policy changes and retention.
ALTER TABLE olp.attempt_usage_facts ADD COLUMN budget_exempt boolean NOT NULL DEFAULT false;
ALTER TABLE olp.attempt_usage_hourly ADD COLUMN budget_exempt boolean NOT NULL DEFAULT false;
ALTER TABLE olp.attempt_usage_hourly DROP CONSTRAINT attempt_usage_hourly_dimensions_key,
 ADD CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
 (bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,budget_group_id,
 attribution,model_family,estimate_provenance,end_user_digest,budget_bucket,budget_exempt);
