package capture

const capturableSink = `s.enabled AND s.project_id IS NULL AND s.format IN ('json','jsonl','otlp')`

const policyJSON = `jsonb_build_object('id',p.id,'project_id',p.project_id,'route_slug',p.route_slug,'sink',p.sink_id,
 'sample_ratio',p.sample_ratio,'include',p.include,'redact',p.redact,'key_ids',p.key_ids,'end_user_digests',p.end_user_digests,
 'max_bytes',p.max_bytes,'enabled',p.enabled,'etag',p.etag,
 'redactor_revisions',COALESCE((SELECT jsonb_agg(jsonb_build_object('name',b.name,'guardrail_id',v.guardrail_id,'revision_id',v.id,'revision',v.revision,'type',g.type) ORDER BY b.position)
 FROM olp.capture_redactors b JOIN olp.guardrail_revisions v ON v.id=b.revision_id JOIN olp.guardrails g ON g.id=v.guardrail_id WHERE b.policy_id=p.id),'[]'::jsonb))`

const (
	sinkOptionsSQL         = `SELECT s.id::text,s.name,s.type FROM olp.export_sinks s WHERE ` + capturableSink + ` ORDER BY s.id LIMIT 1001`
	configurationSQL       = `SELECT enabled,etag::text FROM olp.capture_configuration WHERE singleton`
	lockConfigurationSQL   = configurationSQL + ` FOR UPDATE`
	updateConfigurationSQL = `UPDATE olp.capture_configuration SET enabled=$1,etag=$2,updated_by=$3,updated_at=now() WHERE singleton`
	listPoliciesSQL        = `SELECT ` + policyJSON + ` FROM olp.capture_policies p
 WHERE p.id<$1::uuid AND ($2 OR p.project_id=ANY($3::uuid[])) ORDER BY p.id DESC LIMIT $4`
	getPolicySQL    = `SELECT ` + policyJSON + ` FROM olp.capture_policies p WHERE p.id=$1`
	lockPolicySQL   = getPolicySQL + ` FOR UPDATE`
	createPolicySQL = `INSERT INTO olp.capture_policies(id,project_id,route_slug,sink_id,sample_ratio,include,redact,max_bytes,enabled,etag,created_by,key_ids,end_user_digests)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	updatePolicySQL  = `UPDATE olp.capture_policies SET sink_id=$2,sample_ratio=$3,include=$4,redact=$5,max_bytes=$6,enabled=$7,etag=$8,key_ids=$9,end_user_digests=$10,updated_at=now() WHERE id=$1`
	deletePolicySQL  = `DELETE FROM olp.capture_policies WHERE id=$1`
	countPoliciesSQL = `SELECT count(*) FROM olp.capture_policies`
	sinkScopeSQL     = `SELECT s.project_id IS NULL AND s.enabled,` + capturableSink + ` FROM olp.export_sinks s WHERE s.id=$1 FOR SHARE`
	keyProjectsSQL   = `SELECT id::text,project_id::text FROM olp.api_keys WHERE id=ANY($1::uuid[]) FOR SHARE`
	pinRedactorsSQL  = `SELECT n.pos,n.name,$1::text,g.id::text,v.id::text,v.revision,g.type,v.configuration,v.timeout_ms,v.on_error
 FROM unnest($2::text[]) WITH ORDINALITY n(name,pos)
 JOIN olp.guardrails g ON g.project_id=$1::uuid AND lower(g.name)=lower(n.name) AND g.retired_at IS NULL
 JOIN olp.guardrail_revisions v ON v.id=g.active_revision_id AND v.guardrail_id=g.id
 JOIN olp.guardrail_revision_activations a ON a.revision_id=v.id
 ORDER BY n.pos FOR SHARE OF g`
	clearPinsSQL  = `DELETE FROM olp.capture_redactors WHERE policy_id=$1`
	insertPinsSQL = `INSERT INTO olp.capture_redactors(policy_id,position,name,revision_id)
 SELECT $1,n.pos-1,n.name,n.revision FROM unnest($2::text[],$3::uuid[]) WITH ORDINALITY n(name,revision,pos)`
	loadPoliciesSQL = `SELECT ` + policyJSON + `,COALESCE(p.project_id,(SELECT project_id FROM olp.routes WHERE slug=p.route_slug))::text
 FROM olp.capture_policies p JOIN olp.export_sinks s ON s.id=p.sink_id
 WHERE p.enabled AND ` + capturableSink + ` AND EXISTS(SELECT 1 FROM olp.capture_configuration WHERE singleton AND enabled)
 ORDER BY p.route_slug IS NULL,p.id LIMIT 1000`
	loadDefinitionsSQL = `SELECT DISTINCT v.id::text,g.id::text,COALESCE(g.project_id::text,''),g.type,v.configuration,v.timeout_ms,v.on_error
 FROM olp.capture_redactors b JOIN olp.guardrail_revisions v ON v.id=b.revision_id JOIN olp.guardrails g ON g.id=v.guardrail_id
 WHERE b.policy_id IN (SELECT p.id FROM olp.capture_policies p JOIN olp.export_sinks s ON s.id=p.sink_id WHERE p.enabled AND ` + capturableSink + `)`
	routeSinkSQL = `SELECT s.id::text,s.type,s.format,s.destination,s.settings,s.credential_id::text FROM olp.capture_policies p
 JOIN olp.export_sinks s ON s.id=p.sink_id WHERE p.id=$1 AND p.etag=$2 AND p.enabled AND ` + capturableSink + `
 AND EXISTS(SELECT 1 FROM olp.capture_configuration WHERE singleton AND enabled) FOR SHARE OF p,s`
	auditSQL = `INSERT INTO olp.audit(id,action,resource_type,resource_id,outcome,user_agent_family,project_id)
 VALUES($1,$2,'payload_capture',$3,$4,'other',$5)`
)
