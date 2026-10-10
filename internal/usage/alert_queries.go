package usage

const signalRulesSQL = `SELECT r.id::text,r.event,r.name,r.project_id::text,r.configuration,r.created_at
 FROM olp.notification_rules r JOIN olp.notification_destinations d ON d.id=r.destination_id
 WHERE r.enabled AND d.enabled AND r.event NOT IN ('budget.threshold','provider.grant.lapsed','key.expiring')
 ORDER BY r.id LIMIT 1001`
const signalClockSQL = `SELECT now()`
const createSignalStateSQL = `INSERT INTO olp.notification_signal_states(rule_id,subject) VALUES($1,$2) ON CONFLICT DO NOTHING`
const lockSignalStateSQL = `SELECT active,incident,last_trigger_at FROM olp.notification_signal_states WHERE rule_id=$1 AND subject=$2 FOR UPDATE`
const updateSignalStateSQL = `UPDATE olp.notification_signal_states SET active=$3,incident=$4,last_trigger_at=$5,checked_at=now() WHERE rule_id=$1 AND subject=$2`
const insertSignalDeliverySQL = `INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
 VALUES($1,$2,$3,$4,$5,$6,'pending') ON CONFLICT DO NOTHING`

const providerErrorSignalsSQL = `SELECT p.id::text,p.name,count(a.id),
 CASE WHEN count(a.id)>0 THEN count(a.id) FILTER(WHERE a.error_class IS NOT NULL OR a.status_code>=400)::numeric/count(a.id) END::text
 FROM olp.providers p LEFT JOIN olp.attempts a ON a.provider_id=p.id AND a.completed_at>=now()-make_interval(secs=>$1)
 AND NOT(a.status_code IS NULL AND COALESCE(a.error_class,'') IN ('rate_limit','budget'))
 WHERE p.state='active' GROUP BY p.id,p.name ORDER BY p.id LIMIT 10001`

const credentialFailureSignalsSQL = `WITH recent AS (
 SELECT a.routing->>'credential_version_id' AS credential_id,a.provider_id,a.completed_at,
 (a.status_code IN (401,403) OR a.error_class='credential') AS failed,
 (a.status_code BETWEEN 200 AND 399 AND a.error_class IS NULL) AS succeeded
 FROM olp.attempts a WHERE a.completed_at>=now()-make_interval(secs=>$1) AND a.routing->>'credential_version_id' IS NOT NULL
 AND NOT(a.status_code IS NULL AND COALESCE(a.error_class,'') IN ('rate_limit','budget'))
 ), successes AS (
 SELECT credential_id,max(completed_at) FILTER(WHERE succeeded) AS last_success FROM recent GROUP BY credential_id
 ) SELECT r.credential_id,r.provider_id::text,count(*) FILTER(WHERE r.failed AND (s.last_success IS NULL OR r.completed_at>s.last_success)),count(*)
 FROM recent r JOIN successes s USING(credential_id) GROUP BY r.credential_id,r.provider_id ORDER BY r.credential_id LIMIT 10001`

const routeLatencySignalsSQL = `SELECT rt.slug,rt.project_id::text,
 count(r.id) FILTER(WHERE CASE WHEN $3='ttft' THEN first_output.milliseconds ELSE r.total_latency_ms END IS NOT NULL),
 percentile_cont(0.95) WITHIN GROUP(ORDER BY CASE WHEN $3='ttft' THEN first_output.milliseconds ELSE r.total_latency_ms END)::text
 FROM olp.routes rt LEFT JOIN olp.requests r ON r.route_slug=rt.slug AND r.completed_at>=now()-make_interval(secs=>$1)
 AND r.origin='caller' AND ($2::uuid IS NULL OR ` + requestProjectExpression + `=$2)
 LEFT JOIN LATERAL (
 SELECT min(EXTRACT(epoch FROM a.started_at-r.started_at)*1000+(a.routing->>'first_output_ms')::numeric) AS milliseconds
 FROM olp.attempts a WHERE a.request_id=r.id AND a.request_started_at=r.started_at AND a.committed
 AND a.routing->>'first_output_ms' IS NOT NULL
 ) first_output ON true
 WHERE rt.state='active' AND ($2::uuid IS NULL OR rt.project_id=$2 OR rt.project_id IS NULL)
 GROUP BY rt.slug,rt.project_id ORDER BY rt.slug LIMIT 10001`

const workerStaleSignalsSQL = `SELECT task,GREATEST(0,EXTRACT(epoch FROM now()-COALESCE(last_success_at,first_seen_at)))::text FROM olp.worker_task_health ORDER BY task`
const runtimeInstallSignalsSQL = `SELECT gateway_instance,desired_generation,installed_generation,failed FROM olp.runtime_install_status
 WHERE checked_at>=now()-interval '2 minutes' ORDER BY gateway_instance LIMIT 10001`
const providerCircuitSubjectsSQL = `SELECT id::text,name FROM olp.providers WHERE state='active' ORDER BY id LIMIT 10001`
const activeSignalStatesSQL = `SELECT subject,incident,last_trigger_at FROM olp.notification_signal_states WHERE rule_id=$1 AND active ORDER BY subject LIMIT 10001 FOR UPDATE`
const expireRuntimeInstallStatusSQL = `DELETE FROM olp.runtime_install_status WHERE checked_at<now()-interval '1 day'`
const routedRetirementModelsSQL = `SELECT DISTINCT rt.slug,rt.project_id::text,p.id::text,p.kind,pr.configuration->>'vendor_id',t->>'provider_model'
 FROM olp.routes rt JOIN olp.route_revisions rr ON rr.id=rt.latest_revision_id
 CROSS JOIN LATERAL jsonb_array_elements(rr.targets) t JOIN olp.providers p ON p.id=(t->>'provider_id')::uuid
 JOIN olp.provider_revisions pr ON pr.id=p.active_revision_id
 WHERE rt.state='active' AND ($1::uuid IS NULL OR rt.project_id=$1) ORDER BY rt.slug,p.id::text,t->>'provider_model' LIMIT 10001`

const exhaustedBudgetSignalsSQL = `WITH end_users AS (
 SELECT a.id,COALESCE(a.project_id,k.project_id) AS project_id,a.end_user_digest,
 CASE WHEN a.api_key_id IS NULL THEN p.end_user_policy ELSE k.policy->'end_user_policy' END AS policy,p.limit_templates
 FROM olp.end_user_accounts a LEFT JOIN olp.api_keys k ON k.id=a.api_key_id
 LEFT JOIN olp.projects p ON p.id=COALESCE(a.project_id,k.project_id)
 ), subjects AS (
 SELECT 'api_key'::text AS kind,k.id,k.id AS owner_id,k.project_id,k.name,k.effective_limits AS policy
 FROM olp.api_keys_with_limits k WHERE k.revoked_at IS NULL
 UNION ALL SELECT 'budget_group',g.id,g.id,g.project_id,g.name,g.effective_limits FROM olp.budget_groups_with_limits g
 UNION ALL
 SELECT CASE WHEN a.organization_id IS NOT NULL THEN 'organization' WHEN a.project_id IS NOT NULL THEN 'project' ELSE 'installation' END,
 COALESCE(a.organization_id,a.project_id,a.installation_id),a.id,a.project_id,COALESCE(o.name,p.name,'Installation'),
 COALESCE(o.budget_policy,p.budget_policy,i.budget_policy)
 FROM olp.aggregate_budget_accounts a LEFT JOIN olp.organizations o ON o.id=a.organization_id
 LEFT JOIN olp.projects p ON p.id=a.project_id LEFT JOIN olp.installation i ON i.id=a.installation_id WHERE a.api_key_id IS NULL
 UNION ALL SELECT 'end_user',e.id,e.id,e.project_id,e.end_user_digest,
 olp.effective_limits(COALESCE(e.policy->'overrides'->e.end_user_digest,e.policy->'defaults'),e.limit_templates->(e.policy->>'limit_template'))
 FROM end_users e
 ), current AS (
 SELECT s.*,k.window_kind,b.window_id,COALESCE(w.accrued,0) AS accrued,
 s.policy->>CASE k.window_kind WHEN 'day' THEN 'daily_cost_limit' WHEN 'week' THEN 'weekly_cost_limit' ELSE 'monthly_cost_limit' END AS base_limit,
 (k.window_kind<>'week' OR COALESCE(w.weekly_complete,false)) AS known
 FROM subjects s CROSS JOIN unnest(ARRAY['day','week','month']) AS k(window_kind)
 CROSS JOIN LATERAL olp.budget_window(k.window_kind,now()) b
 LEFT JOIN LATERAL (
 SELECT accrued,weekly_complete FROM olp.api_key_cost_windows WHERE s.kind='api_key' AND api_key_id=s.owner_id AND window_kind=k.window_kind AND window_id=b.window_id
 UNION ALL SELECT accrued,weekly_complete FROM olp.budget_group_cost_windows WHERE s.kind='budget_group' AND budget_group_id=s.owner_id AND window_kind=k.window_kind AND window_id=b.window_id
 UNION ALL SELECT accrued,weekly_complete FROM olp.end_user_cost_windows WHERE s.kind='end_user' AND account_id=s.owner_id AND window_kind=k.window_kind AND window_id=b.window_id
 UNION ALL SELECT accrued,weekly_complete FROM olp.aggregate_cost_windows WHERE s.kind IN ('organization','project','installation') AND account_id=s.owner_id AND window_kind=k.window_kind AND window_id=b.window_id
 ) w ON true WHERE ($1::uuid IS NULL OR s.project_id=$1)
 ) SELECT c.kind,c.id::text,c.project_id::text,c.name,c.window_kind,c.window_id,c.accrued::text,
 (c.base_limit::numeric+COALESCE((SELECT sum(amount) FROM olp.budget_increases bi WHERE bi.owner_id=c.owner_id
 AND bi.window_kind=c.window_kind AND bi.window_id=c.window_id AND bi.revoked_at IS NULL AND bi.starts_at<=now() AND bi.expires_at>now()),0))::text,c.known
 FROM current c WHERE c.base_limit IS NOT NULL AND c.base_limit::numeric>0 ORDER BY c.kind,c.id,c.window_kind LIMIT 10001`

const reportPeriodSQL = `SELECT date_trunc($1,now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS end_at,
 (date_trunc($1,now() AT TIME ZONE 'UTC')-CASE $1 WHEN 'day' THEN interval '1 day' WHEN 'week' THEN interval '1 week' ELSE interval '1 month' END) AT TIME ZONE 'UTC' AS start_at`
const reportAlreadyQueuedSQL = `SELECT EXISTS(SELECT 1 FROM olp.notification_deliveries WHERE rule_id=$1 AND dedup_key=$2 AND NOT resolved)`
const notificationCurrencySQL = `SELECT currency FROM olp.pricing_currency WHERE singleton`

const pendingChannelDeliverySQL = `SELECT v.id::text,v.rule_id::text,
 CASE WHEN v.api_key_id IS NOT NULL THEN 'key.expiring' ELSE COALESCE(v.event,r.event) END,v.attempts,v.last_attempt_at,
 r.name,r.subject_kind,r.subject_id::text,r.window_kind,v.window_id,v.threshold_percent,
 v.accrued::text,v.limit_amount::text,COALESCE(v.currency::text,''),v.payload,v.dedup_key,v.resolved
 FROM olp.notification_deliveries v JOIN olp.notification_rules r ON r.id=v.rule_id
 JOIN olp.notification_destinations d ON d.id=r.destination_id
 WHERE v.status IN ('pending','failed') AND v.attempts<$1 AND r.enabled AND d.enabled
 AND (v.last_attempt_at IS NULL OR v.attempts<1 OR v.last_attempt_at+make_interval(mins=>1<<GREATEST(v.attempts-1,0))<=now())
 AND (v.dedup_key IS NULL OR NOT EXISTS(
 SELECT 1 FROM olp.notification_deliveries earlier JOIN olp.notification_rules er ON er.id=earlier.rule_id
 WHERE er.destination_id=r.destination_id AND er.enabled AND earlier.status IN ('pending','failed') AND earlier.attempts<$1
 AND COALESCE(earlier.payload->>'incident_key',earlier.dedup_key)=COALESCE(v.payload->>'incident_key',v.dedup_key)
 AND (earlier.created_at,earlier.id)<(v.created_at,v.id)))
 ORDER BY v.created_at,v.id LIMIT $2`
const readChannelDestinationSQL = `SELECT d.url,d.secret_id::text,d.type,d.configuration FROM olp.notification_rules r
 JOIN olp.notification_destinations d ON d.id=r.destination_id WHERE r.id=$1 AND r.enabled AND d.enabled FOR SHARE OF r,d`
const lockNotificationIncidentSQL = `SELECT pg_try_advisory_lock(hashtextextended($1,502))`
const unlockNotificationIncidentSQL = `SELECT pg_advisory_unlock(hashtextextended($1,502))`
const signalDeliveryCurrentSQL = `SELECT EXISTS(SELECT 1 FROM olp.notification_signal_states
 WHERE rule_id=$1 AND subject=$2 AND active AND incident=$3)`
