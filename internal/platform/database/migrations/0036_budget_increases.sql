-- Grants never replace a permanent policy or its accrued/pending balances.
CREATE TABLE olp.budget_increases (
 id uuid PRIMARY KEY,
 owner_id uuid NOT NULL,
 project_id uuid REFERENCES olp.projects(id) ON DELETE CASCADE,
 target jsonb NOT NULL CHECK(jsonb_typeof(target)='object'),
 window_kind text NOT NULL CHECK(window_kind IN ('day','week','month')),
 window_id bigint NOT NULL CHECK(window_id>=0),
 amount numeric(24,12) NOT NULL CHECK(amount>0),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 256),
 starts_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK(expires_at>starts_at),
 window_ends_at timestamptz NOT NULL CHECK(expires_at<=window_ends_at),
 created_at timestamptz NOT NULL DEFAULT now(),
 created_by uuid NOT NULL REFERENCES olp.users(id),
 revoked_at timestamptz,
 revoked_by uuid REFERENCES olp.users(id),
 etag uuid NOT NULL
);
CREATE INDEX budget_increases_active_owner ON olp.budget_increases(owner_id,expires_at) WHERE revoked_at IS NULL;
CREATE INDEX budget_increases_project ON olp.budget_increases(project_id,id DESC);

-- Reports retain permanent editable limits and expose effective allowances
-- separately, including template ceilings and active temporary exceptions.
CREATE FUNCTION olp.budget_allowances(owner uuid, effective jsonb, budget jsonb)
RETURNS jsonb LANGUAGE plpgsql STABLE AS $$
DECLARE kind text; field text; cap numeric; extra numeric; expiry timestamptz; win bigint; detail jsonb;
BEGIN
 FOR kind,field IN SELECT * FROM (VALUES('day','daily'),('week','weekly'),('month','monthly')) v(k,f) LOOP
  cap:=(effective->>(field||'_cost_limit'))::numeric;
  SELECT window_id INTO win FROM olp.budget_window(kind,now());
  SELECT COALESCE(sum(amount),0),min(expires_at) INTO extra,expiry FROM olp.budget_increases
   WHERE owner_id=owner AND window_kind=kind AND window_id=win AND revoked_at IS NULL AND starts_at<=now() AND expires_at>now() AND cap IS NOT NULL;
  detail:=COALESCE(budget->field,'{}'::jsonb)||jsonb_build_object('effective_limit',(cap+extra)::text,'temporary_increase',extra::text,'increase_expires_at',expiry);
  IF detail ? 'remaining' THEN
   detail:=detail||jsonb_build_object('remaining',CASE WHEN cap IS NULL THEN NULL ELSE GREATEST(cap+extra-(detail->>'accrued')::numeric,0)::text END);
  END IF;
  budget:=jsonb_set(budget,ARRAY[field],detail);
 END LOOP;
 RETURN budget;
END $$;
