CREATE OR REPLACE FUNCTION olp.code_account_available(a olp.code_accounts) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT (a.health NOT IN ('unavailable','quota_limited') OR a.unavailable_until<=now())
      AND NOT EXISTS (
        SELECT 1 FROM (VALUES
          ('remaining_tokens', 'token_observation'),
          ('remaining_requests', 'request_observation')
        ) AS counts(remaining, observation)
        WHERE (a.allowance->>counts.remaining)::bigint=0
          AND NOT coalesce((CASE WHEN a.allowance ? counts.observation
            THEN a.allowance->counts.observation->>'resets_at'
            ELSE a.allowance->>'resets_at' END)::timestamptz<=now(),false))
      AND NOT (coalesce((a.allowance->>'remaining_percent')::numeric=0,false)
        AND NOT coalesce((a.allowance->>'resets_at')::timestamptz<=now(),false))
      AND NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(coalesce(a.allowance->'windows','[]'::jsonb)) w
        WHERE (w->>'remaining_percent')::numeric=0
          AND NOT coalesce((w->>'resets_at')::timestamptz<=now(),false))
$$;
