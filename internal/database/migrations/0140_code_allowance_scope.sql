CREATE OR REPLACE FUNCTION olp.code_account_available(a olp.code_accounts, band_ordering boolean, requested_family text) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT a.health NOT IN ('unavailable','quota_limited') OR coalesce(a.unavailable_until<=now(),false)
$$;

CREATE FUNCTION olp.code_model_cooling(account_id uuid, requested_model text) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM olp.code_account_cooldowns cd
        WHERE cd.account_id=$1 AND cd.retry_at>now() AND (cd.model IS NULL OR cd.model=$2)
    )
$$;
