package access

const auditInsertSQL = `WITH subject AS (
 SELECT CASE WHEN $6::text~'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN $6::uuid END AS id
 ) INSERT INTO olp.audit(id,actor_user_id,actor_management_token_id,action,resource_type,resource_id,outcome,source_ip,user_agent_family,project_id)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,COALESCE($10::uuid,CASE $5
 WHEN 'project' THEN (SELECT p.id FROM olp.projects p WHERE p.id=subject.id)
 WHEN 'api_key' THEN (SELECT k.project_id FROM olp.api_keys k WHERE k.id=subject.id)
 WHEN 'budget_group' THEN (SELECT g.project_id FROM olp.budget_groups g WHERE g.id=subject.id)
 WHEN 'provider' THEN (SELECT p.project_id FROM olp.providers p WHERE p.id=subject.id)
 WHEN 'provider_credential' THEN (SELECT p.project_id FROM olp.provider_credentials c JOIN olp.providers p ON p.id=c.provider_id WHERE c.id=subject.id)
 WHEN 'provider_slot' THEN (SELECT p.project_id FROM olp.provider_slots s JOIN olp.providers p ON p.id=s.provider_id WHERE s.id=subject.id)
 WHEN 'provider_model' THEN (SELECT p.project_id FROM olp.provider_models m JOIN olp.providers p ON p.id=m.provider_id WHERE m.id=subject.id)
 WHEN 'route' THEN (SELECT r.project_id FROM olp.routes r WHERE r.id=subject.id)
 WHEN 'route_draft' THEN (SELECT d.project_id FROM olp.route_drafts d WHERE d.id=subject.id)
 WHEN 'route_revision' THEN (SELECT r.project_id FROM olp.route_revisions rr JOIN olp.routes r ON r.id=rr.route_id WHERE rr.id=subject.id)
 WHEN 'notification_destination' THEN (SELECT d.project_id FROM olp.notification_destinations d WHERE d.id=subject.id)
 WHEN 'notification_rule' THEN (SELECT r.project_id FROM olp.notification_rules r WHERE r.id=subject.id)
 WHEN 'export_sink' THEN (SELECT s.project_id FROM olp.export_sinks s WHERE s.id=subject.id)
 WHEN 'capture_policy' THEN (SELECT p.project_id FROM olp.capture_policies p WHERE p.id=subject.id)
 END) FROM subject`
