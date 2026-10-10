package access

const createChannelDestinationSQL = `INSERT INTO olp.notification_destinations(id,name,url,project_id,etag,created_by,enabled,type,configuration)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`
const updateChannelDestinationSQL = `UPDATE olp.notification_destinations SET name=$2,url=$3,enabled=$4,etag=$5,type=$6,configuration=$7,updated_at=now() WHERE id=$1`
const createChannelRuleSQL = `INSERT INTO olp.notification_rules(id,name,project_id,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,enabled,etag,created_by,configuration)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
const updateChannelRuleSQL = `UPDATE olp.notification_rules SET name=$2,subject_kind=$3,subject_id=$4,window_kind=$5,threshold_percent=$6,destination_id=$7,enabled=$8,etag=$9,configuration=$10,updated_at=now() WHERE id=$1`
const genericRuleCountSQL = `SELECT count(*) FROM olp.notification_rules WHERE event NOT IN ('budget.threshold','provider.grant.lapsed','key.expiring')`
const lockGenericRuleCreationSQL = `SELECT pg_advisory_xact_lock(hashtextextended('olp.notification_rule_creation',0))`
const readDestinationSecretSQL = `SELECT secret_id::text FROM olp.notification_destinations WHERE id=$1`
