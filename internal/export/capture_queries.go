package export

const captureSinkOptionsSQL = `SELECT id::text,name,type FROM olp.export_sinks WHERE project_id IS NULL AND enabled ORDER BY id LIMIT 1001`

const captureProjection = `jsonb_build_object('id',p.id,'project_id',p.project_id,'route_slug',p.route_slug,'sink',p.sink_id,
 'sample_ratio',p.sample_ratio,'include',p.include,'redact',p.redact,'key_ids',p.key_ids,'end_user_digests',p.end_user_digests,'max_bytes',p.max_bytes,'enabled',p.enabled,
 'etag',p.etag,'created_by',p.created_by,'created_at',p.created_at,'updated_at',p.updated_at)`
const captureConfigurationSQL = `SELECT enabled,etag::text FROM olp.capture_configuration WHERE singleton`
const lockCaptureConfigurationSQL = `SELECT enabled,etag::text FROM olp.capture_configuration WHERE singleton FOR UPDATE`
const updateCaptureConfigurationSQL = `UPDATE olp.capture_configuration SET enabled=$1,etag=$2,updated_by=$3,updated_at=now() WHERE singleton`
const listCapturePoliciesSQL = `SELECT ` + captureProjection + ` FROM olp.capture_policies p
 WHERE p.id<$1::uuid AND ($2 OR p.project_id=ANY($3::uuid[])) ORDER BY p.id DESC LIMIT $4`
const getCapturePolicySQL = `SELECT ` + captureProjection + `,p.etag::text,p.project_id::text FROM olp.capture_policies p WHERE p.id=$1`
const lockCapturePolicySQL = `SELECT ` + captureProjection + `,p.etag::text,p.project_id::text FROM olp.capture_policies p WHERE p.id=$1 FOR UPDATE`
const createCapturePolicySQL = `INSERT INTO olp.capture_policies(id,project_id,route_slug,sink_id,sample_ratio,include,redact,max_bytes,enabled,etag,created_by,key_ids,end_user_digests)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
const updateCapturePolicySQL = `UPDATE olp.capture_policies SET sink_id=$2,sample_ratio=$3,include=$4,redact=$5,max_bytes=$6,enabled=$7,etag=$8,key_ids=$9,end_user_digests=$10,updated_at=now() WHERE id=$1`
const deleteCapturePolicySQL = `DELETE FROM olp.capture_policies WHERE id=$1`
const countCapturePoliciesSQL = `SELECT count(*) FROM olp.capture_policies`
const captureSinkScopeSQL = `SELECT project_id::text,enabled FROM olp.export_sinks WHERE id=$1 FOR SHARE`
const captureRouteScopeSQL = `SELECT project_id::text FROM olp.routes WHERE slug=$1 FOR SHARE`
const captureKeyScopeSQL = `SELECT project_id::text FROM olp.api_keys WHERE id=$1 FOR SHARE`
const loadCapturePoliciesSQL = `SELECT ` + captureProjection + ` FROM olp.capture_policies p
 JOIN olp.export_sinks s ON s.id=p.sink_id
 WHERE p.enabled AND s.enabled AND s.project_id IS NULL AND EXISTS(SELECT 1 FROM olp.capture_configuration WHERE singleton AND enabled)
 ORDER BY p.id LIMIT 1001`
const readCaptureDestinationSQL = `SELECT s.type,s.destination,s.credential_id::text,s.format FROM olp.capture_policies p
 JOIN olp.export_sinks s ON s.id=p.sink_id WHERE p.id=$1 AND p.etag=$2 AND p.enabled AND s.enabled AND s.project_id IS NULL
 AND EXISTS(SELECT 1 FROM olp.capture_configuration WHERE singleton AND enabled) FOR SHARE OF p,s`
const captureOutcomeAuditSQL = `INSERT INTO olp.audit(id,action,resource_type,resource_id,outcome,user_agent_family,project_id)
 VALUES($1,$2,'payload_capture',$3,$4,'other',$5)`
