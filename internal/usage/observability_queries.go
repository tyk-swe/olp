package usage

const sessionDimensionExpression = `COALESCE(NULLIF(attribution->>'session',''),'unidentified')`
const projectDimensionExpression = `COALESCE((SELECT k.project_id::text FROM olp.api_keys k WHERE k.id=usage_rows.api_key_id),
 (SELECT rt.project_id::text FROM olp.routes rt WHERE rt.slug=usage_rows.route_slug),'unassigned')`
const requestProjectExpression = `COALESCE((SELECT k.project_id FROM olp.api_keys k WHERE k.id=r.api_key_id),
 (SELECT rt.project_id FROM olp.routes rt WHERE rt.slug=r.route_slug))`
