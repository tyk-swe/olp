CREATE FUNCTION olp.code_account_available(a olp.code_accounts, band_ordering boolean, requested_family text) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT (a.health NOT IN ('unavailable','quota_limited') OR coalesce(a.unavailable_until<=now(),false))
      AND NOT EXISTS (
        SELECT 1 FROM (VALUES
          ('remaining_tokens', 'token_observation'),
          ('remaining_requests', 'request_observation')
        ) AS counts(remaining, observation)
        WHERE (a.allowance->>counts.remaining)::bigint=0
          AND NOT coalesce((CASE WHEN a.allowance ? counts.observation
            THEN a.allowance->counts.observation->>'resets_at'
            ELSE a.allowance->>'resets_at' END)::timestamptz<=now(),false)
          AND (NOT band_ordering OR (coalesce(a.allowance->counts.observation->>'observed_at',
            a.allowance->>'observed_at'))::timestamptz>=now()-interval '15 minutes'))
      AND NOT (
        coalesce((a.allowance->>'remaining_percent')::numeric=0,false)
        AND NOT EXISTS (
          SELECT 1 FROM jsonb_array_elements(coalesce(a.allowance->'windows','[]'::jsonb)) w
          WHERE w->>'limit_id'='codex' AND w->>'window'='primary')
        AND NOT coalesce((a.allowance->>'resets_at')::timestamptz<=now(),false)
        AND (NOT band_ordering OR (requested_family='codex'
          AND (a.allowance->>'observed_at')::timestamptz>=now()-interval '15 minutes')))
      AND NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(coalesce(a.allowance->'windows','[]'::jsonb)) w
        WHERE coalesce(w->>'evidence','passive')='passive'
          AND (NOT band_ordering OR w->>'limit_id'=requested_family)
          AND (CASE coalesce(w->>'unit','percent')
            WHEN 'percent' THEN (w->>'remaining_percent')::numeric=0
            WHEN 'count' THEN (w->>'remaining_value')::numeric=0
            WHEN 'credits' THEN (w->>'remaining_value')::numeric=0
            WHEN 'cents' THEN (w->>'remaining_value')::numeric=0
            ELSE false END)
          AND NOT coalesce((w->>'resets_at')::timestamptz<=now(),false)
          AND (NOT band_ordering OR (w->>'observed_at')::timestamptz>=now()-interval '15 minutes'))
$$;

CREATE OR REPLACE FUNCTION olp.code_account_available(a olp.code_accounts) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT olp.code_account_available(a, false, NULL)
$$;
