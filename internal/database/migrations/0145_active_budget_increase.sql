CREATE FUNCTION olp.active_budget_increase(owner uuid, kind text, win bigint, at timestamptz)
RETURNS numeric LANGUAGE sql STABLE AS $$
 SELECT COALESCE(sum(amount),0) FROM olp.budget_increases
 WHERE owner_id=owner AND window_kind=kind AND window_id=win AND revoked_at IS NULL AND starts_at<=at AND expires_at>at
$$;

ALTER FUNCTION olp.budget_windows(timestamptz) ROWS 1;
