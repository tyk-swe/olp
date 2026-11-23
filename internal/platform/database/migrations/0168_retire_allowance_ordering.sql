CREATE OR REPLACE FUNCTION olp.code_account_available(a olp.code_accounts) RETURNS boolean
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
            ELSE a.allowance->>'resets_at' END)::timestamptz<=now(),false))
      AND NOT (
        coalesce((a.allowance->>'remaining_percent')::numeric=0,false)
        AND NOT EXISTS (
          SELECT 1 FROM jsonb_array_elements(coalesce(a.allowance->'windows','[]'::jsonb)) w
          WHERE w->>'limit_id'='codex' AND w->>'window'='primary')
        AND NOT coalesce((a.allowance->>'resets_at')::timestamptz<=now(),false))
      AND NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(coalesce(a.allowance->'windows','[]'::jsonb)) w
        WHERE coalesce(w->>'evidence','passive')='passive'
          AND (CASE coalesce(w->>'unit','percent')
            WHEN 'percent' THEN (w->>'remaining_percent')::numeric=0
            WHEN 'count' THEN (w->>'remaining_value')::numeric=0
            WHEN 'credits' THEN (w->>'remaining_value')::numeric=0
            WHEN 'cents' THEN (w->>'remaining_value')::numeric=0
            ELSE false END)
          AND NOT coalesce((w->>'resets_at')::timestamptz<=now(),false))
$$;

DROP FUNCTION olp.code_account_available(olp.code_accounts, boolean, text);

ALTER TABLE olp.code_decisions DROP COLUMN band, DROP COLUMN evidence, DROP COLUMN evidence_observed_at;

UPDATE olp.code_decisions
SET skipped = (SELECT coalesce(jsonb_agg(e), '[]'::jsonb) FROM jsonb_array_elements(skipped) e WHERE e->>'class' <> 'band')
WHERE skipped @> '[{"class":"band"}]';
