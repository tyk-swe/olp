ALTER TABLE olp.code_attempts
    ADD COLUMN upstream_status integer CHECK (upstream_status BETWEEN 100 AND 599),
    ADD COLUMN outcome_origin text CHECK (outcome_origin IN ('upstream','gateway','client')),
    ADD COLUMN outcome text CHECK (outcome IN ('headers','completed','incomplete','failed','rejected','interrupted','canceled','transport_error')),
    ADD COLUMN outcome_observed_at timestamptz,
    ADD CONSTRAINT code_outcome_observation CHECK (
        (outcome_origin IS NULL AND outcome IS NULL AND outcome_observed_at IS NULL AND upstream_status IS NULL)
        OR (outcome_origin IS NOT NULL AND outcome IS NOT NULL AND outcome_observed_at IS NOT NULL));

CREATE OR REPLACE FUNCTION olp.code_account_available(a olp.code_accounts) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT (a.health NOT IN ('unavailable','quota_limited') OR a.unavailable_until<=now())
      AND (a.allowance IS NULL
        OR coalesce((a.allowance->>'resets_at')::timestamptz<=now(),false)
        OR NOT (coalesce((a.allowance->>'remaining_tokens')::bigint=0,false)
          OR coalesce((a.allowance->>'remaining_requests')::bigint=0,false)
          OR coalesce((a.allowance->>'remaining_percent')::numeric=0,false)))
      AND NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(coalesce(a.allowance->'windows','[]'::jsonb)) w
        WHERE (w->>'remaining_percent')::numeric=0
          AND NOT coalesce((w->>'resets_at')::timestamptz<=now(),false))
$$;
