CREATE OR REPLACE FUNCTION olp.commitment_hourly_allocation(h olp.attempt_usage_hourly) RETURNS numeric
LANGUAGE sql STABLE AS $$
 SELECT SUM(COALESCE(a.allocated_cost,0)) FROM olp.commitment_usage u JOIN olp.commitment_periods cp ON cp.id=u.period_id AND NOT cp.legacy LEFT JOIN olp.commitment_allocations a ON a.attempt_id=u.attempt_id
 WHERE u.observed_at >= (h).bucket AND u.observed_at < (h).bucket+interval '1 hour'
 AND NOT EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE f.attempt_id=u.attempt_id)
 AND u.payload->>'provider_id' IS NOT DISTINCT FROM (h).provider_id::text
 AND u.payload->>'route_slug' IS NOT DISTINCT FROM (h).route_slug
 AND u.payload->>'upstream_model' IS NOT DISTINCT FROM (h).upstream_model
 AND u.payload->>'operation' IS NOT DISTINCT FROM (h).operation
 AND u.payload->>'surface' IS NOT DISTINCT FROM (h).surface
 AND u.payload->>'api_key_id' IS NOT DISTINCT FROM (h).api_key_id::text
 AND u.payload->>'budget_group_id' IS NOT DISTINCT FROM (h).budget_group_id::text
 AND u.payload->'attribution' IS NOT DISTINCT FROM (h).attribution
 AND u.payload->>'end_user_digest' IS NOT DISTINCT FROM (h).end_user_digest
 AND u.payload->>'model_family' IS NOT DISTINCT FROM (h).model_family
 AND u.payload->>'estimate_provenance' IS NOT DISTINCT FROM (h).estimate_provenance
 AND (u.payload->>'budget_exempt')::boolean=(h).budget_exempt
 AND ((h).reconcil_caller_attempts IS NULL
      OR COALESCE((u.payload->>'caller_credential')::boolean,false)=(h).caller_credential)
 AND (u.payload->>'commitment_amount')::numeric IS NOT DISTINCT FROM (h).commitment_amount
 AND u.payload->>'commitment_period' IS NOT DISTINCT FROM (h).commitment_period
$$;
