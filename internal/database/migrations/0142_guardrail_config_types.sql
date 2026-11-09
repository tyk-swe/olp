DO $$
DECLARE
 stray text;
BEGIN
 WITH allowed(kind, keys) AS (VALUES
  ('model', ARRAY['route', 'rule']),
  ('decision_model', ARRAY['route', 'rule']),
  ('semantic_guard', ARRAY['route', 'semantic']),
  ('plugin', ARRAY['plugin_id', 'plugin_version']),
  ('webhook', ARRAY['endpoint', 'credential_ref', 'network', 'content_egress']),
  ('lakera', ARRAY['endpoint', 'credential_ref', 'network', 'content_egress']),
  ('azure.prompt_shields', ARRAY['endpoint', 'credential_ref', 'network', 'content_egress']),
  ('azure.content_safety', ARRAY['endpoint', 'credential_ref', 'network', 'content_egress', 'threshold']),
  ('bedrock', ARRAY['endpoint', 'credential_ref', 'network', 'content_egress', 'region', 'guardrail', 'version']),
  ('model_armor', ARRAY['endpoint', 'credential_ref', 'network', 'content_egress', 'template']),
  ('presidio', ARRAY['endpoint', 'credential_ref', 'network', 'content_egress', 'language', 'anonymizer_endpoint']),
  ('hiddenlayer', ARRAY['endpoint', 'credential_ref', 'network', 'content_egress', 'parameters', 'auth_endpoint'])
 ), live AS (
  SELECT g.id::text AS guardrail, g.type AS kind, v.configuration
  FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.guardrail_id = g.id
  WHERE v.id = g.active_revision_id OR v.id IN (
   SELECT (entry->>'revision_id')::uuid FROM olp.guardrail_policy_revisions p, jsonb_array_elements(p.entries) entry
   WHERE p.id IN (SELECT active_revision_id FROM olp.guardrail_policies UNION SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments)
    OR p.policy_id IN (SELECT id FROM olp.guardrail_policies WHERE managed_route_revision_id IN (SELECT latest_revision_id FROM olp.routes)))
  UNION ALL
  SELECT d.value->>'guardrail_id', d.value->>'type', d.value->'configuration'
  FROM (SELECT snapshot::jsonb AS snapshot FROM olp.runtime_releases ORDER BY sequence DESC LIMIT 1) r
  CROSS JOIN LATERAL jsonb_each(COALESCE(r.snapshot->'guardrails'->'definitions', '{}')) d
 )
 SELECT string_agg(DISTINCT live.guardrail || ' ' || live.kind || '.' || key, ', ') INTO stray
 FROM live LEFT JOIN allowed USING (kind) CROSS JOIN LATERAL jsonb_object_keys(live.configuration) key
 WHERE live.kind NOT LIKE 'builtin.%' AND live.kind <> 'typesafe'
  AND key <> ALL (COALESCE(allowed.keys, ARRAY['endpoint', 'credential_ref', 'network', 'content_egress', 'parameters']));
 IF stray IS NOT NULL THEN
  RAISE EXCEPTION 'activate guardrail revisions without the configuration fields their type does not accept before upgrading: %', stray;
 END IF;
END $$;
