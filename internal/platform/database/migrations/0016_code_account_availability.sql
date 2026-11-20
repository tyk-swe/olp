ALTER TABLE olp.code_accounts ADD COLUMN unavailable_until timestamptz;

CREATE FUNCTION olp.code_account_available(a olp.code_accounts) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT (a.health NOT IN ('unavailable','quota_limited') OR a.unavailable_until<=now())
      AND (a.allowance IS NULL
        OR coalesce((a.allowance->>'resets_at')::timestamptz<=now(),false)
        OR NOT (coalesce((a.allowance->>'remaining_tokens')::bigint=0,false)
          OR coalesce((a.allowance->>'remaining_requests')::bigint=0,false)
          OR coalesce((a.allowance->>'remaining_percent')::numeric=0,false)))
$$;
