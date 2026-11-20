-- A bounded hierarchy label supports diagnosis without retaining identifiers,
-- caller labels or upstream error bodies. Existing request retention applies.
ALTER TABLE olp.requests ADD COLUMN budget_boundary text NOT NULL DEFAULT ''
 CHECK(budget_boundary IN ('','api_key','budget_group','key_route','key_end_user',
 'project_end_user','attribution','project','organization','installation'));
