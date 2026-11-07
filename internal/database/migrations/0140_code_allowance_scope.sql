CREATE OR REPLACE FUNCTION olp.code_account_available(a olp.code_accounts, band_ordering boolean, requested_family text) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT a.health NOT IN ('unavailable','quota_limited') OR coalesce(a.unavailable_until<=now(),false)
$$;
