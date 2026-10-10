package export

const sinkProjection = `jsonb_build_object('id',s.id,'name',s.name,'project_id',s.project_id,'type',s.type,'destination',s.destination,
 'streams',s.streams,'format',s.format,'filter',s.filter,'enabled',s.enabled,'credential_configured',s.credential_id IS NOT NULL,
 'etag',s.etag,'created_by',s.created_by,'created_at',s.created_at,'updated_at',s.updated_at)`

const listSinksSQL = `SELECT ` + sinkProjection + ` FROM olp.export_sinks s
 WHERE s.id<$1::uuid AND ($2 OR s.project_id=ANY($3::uuid[])) ORDER BY s.id DESC LIMIT $4`

const getSinkSQL = `SELECT ` + sinkProjection + `,s.etag::text,s.project_id::text FROM olp.export_sinks s WHERE s.id=$1`
const lockSinkSQL = `SELECT ` + sinkProjection + `,s.etag::text,s.project_id::text,s.credential_id::text FROM olp.export_sinks s WHERE s.id=$1 FOR UPDATE`
const createSinkSQL = `INSERT INTO olp.export_sinks(id,name,project_id,type,destination,streams,format,filter,enabled,etag,created_by)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
const updateSinkSQL = `UPDATE olp.export_sinks SET name=$2,destination=$3,streams=$4,filter=$5,enabled=$6,etag=$7,updated_at=now() WHERE id=$1`
const setCredentialSQL = `UPDATE olp.export_sinks SET credential_id=$2 WHERE id=$1`
const deleteCredentialSQL = `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`
const deleteSinkSQL = `DELETE FROM olp.export_sinks WHERE id=$1`
const countSinksSQL = `SELECT count(*) FROM olp.export_sinks`
const filterProjectScopeSQL = `SELECT EXISTS(SELECT 1 FROM olp.projects WHERE id=$1)`
const filterRouteScopeSQL = `SELECT project_id::text FROM olp.routes WHERE slug=$1 FOR SHARE`
const listGapsSQL = `SELECT to_jsonb(g) FROM olp.export_gaps g JOIN olp.export_sinks s ON s.id=g.sink_id
 WHERE g.sink_id=$1 AND g.id<$2::uuid AND ($3 OR s.project_id=ANY($4::uuid[])) ORDER BY g.id DESC LIMIT $5`
const listCursorsSQL = `SELECT to_jsonb(c) FROM olp.export_cursors c WHERE c.sink_id=$1 ORDER BY c.stream`
const sinkStatusSQL = `SELECT jsonb_build_object('stream',st.stream,'cursor',c.cursor,'delivered_total',COALESCE(c.delivered_total,0),
 'failures_total',COALESCE(c.failed_total,0),'gaps_total',COALESCE(c.gaps_total,0),'last_success_at',c.last_success_at,
 'last_attempt_at',pending.last_attempt_at,'pending',COALESCE(pending.pending,0),'oldest_pending_at',pending.oldest_pending_at,
 'pending_error_codes',COALESCE(pending.error_codes,'{}'::text[]))
 FROM olp.export_sinks s CROSS JOIN LATERAL unnest(s.streams) st(stream)
 LEFT JOIN olp.export_cursors c ON c.sink_id=s.id AND c.stream=st.stream
 LEFT JOIN LATERAL (SELECT count(*) FILTER(WHERE p.delivered_at IS NULL) AS pending,
 min(r.queued_at) FILTER(WHERE p.delivered_at IS NULL) AS oldest_pending_at,max(p.last_attempt_at) AS last_attempt_at,
 array_agg(DISTINCT p.last_error_code) FILTER(WHERE p.delivered_at IS NULL AND p.last_error_code IS NOT NULL) AS error_codes
 FROM olp.export_pending p JOIN olp.export_records r ON r.id=p.record_id WHERE p.sink_id=s.id AND r.stream=st.stream) pending ON true
 WHERE s.id=$1 ORDER BY st.stream`

const activeSinksSQL = `SELECT id::text,type,destination,credential_id::text,streams,format FROM olp.export_sinks WHERE enabled ORDER BY id LIMIT 1001`
const lockStreamSQL = `SELECT pg_try_advisory_xact_lock(hashtextextended($1,501))`
const readDestinationSQL = `SELECT type,destination,credential_id::text,format FROM olp.export_sinks WHERE id=$1 AND enabled AND $2=ANY(streams) FOR SHARE`
const pendingRecordsSQL = `SELECT r.id::text,r.stream,r.occurred_at,r.queued_at,
 jsonb_build_object('version',1,'event_id',r.id,'stream',r.stream,'source_id',r.source_id,'project_id',r.project_id,
 'route',r.route,'outcome',r.outcome,'occurred_at',r.occurred_at,'data',r.payload),p.attempts
 FROM olp.export_pending p JOIN olp.export_records r ON r.id=p.record_id
 WHERE p.sink_id=$1 AND r.stream=$2 AND p.delivered_at IS NULL AND p.next_attempt_at<=now()
 ORDER BY r.queued_at,r.id LIMIT $3`
const claimRecordSQL = `UPDATE olp.export_pending SET attempts=LEAST(attempts+1,2147483647),
 last_attempt_at=now(),next_attempt_at=now()+interval '2 minutes' WHERE sink_id=$1 AND record_id=$2 AND delivered_at IS NULL AND attempts=$3`
const finishRecordSQL = `UPDATE olp.export_pending SET delivered_at=CASE WHEN $3 THEN now() ELSE NULL END,
 last_error_code=$4,next_attempt_at=now()+make_interval(secs=>$5)
 WHERE sink_id=$1 AND record_id=$2 AND delivered_at IS NULL`
const checkpointCursorSQL = `INSERT INTO olp.export_cursors(sink_id,stream,cursor,delivered_total,failed_total,last_success_at)
 VALUES($1,$2,$3,$4,$5,CASE WHEN $4::bigint>0 THEN now() END) ON CONFLICT(sink_id,stream) DO UPDATE SET
 cursor=COALESCE(EXCLUDED.cursor,export_cursors.cursor),
 delivered_total=export_cursors.delivered_total+EXCLUDED.delivered_total,
 failed_total=export_cursors.failed_total+EXCLUDED.failed_total,last_success_at=COALESCE(EXCLUDED.last_success_at,export_cursors.last_success_at),updated_at=now()`
const maintainSQL = `SELECT olp.maintain_exports($1)`
const exportMetricsSQL = `SELECT s.id::text,st.stream,
 COALESCE(EXTRACT(epoch FROM now()-min(r.queued_at) FILTER(WHERE p.delivered_at IS NULL)),0)::double precision,
 COALESCE(c.delivered_total,0),COALESCE(c.failed_total,0)
 FROM olp.export_sinks s CROSS JOIN LATERAL unnest(s.streams) AS st(stream)
 LEFT JOIN olp.export_cursors c ON c.sink_id=s.id AND c.stream=st.stream
 LEFT JOIN olp.export_pending p ON p.sink_id=s.id
 LEFT JOIN olp.export_records r ON r.id=p.record_id AND r.stream=st.stream
 WHERE s.enabled GROUP BY s.id,st.stream,c.delivered_total,c.failed_total ORDER BY s.id,st.stream LIMIT 5001`
const exportDeliveryMetricsSQL = `SELECT s.id::text,COALESCE(sum(c.delivered_total),0)::bigint,COALESCE(sum(c.failed_total),0)::bigint,
 COALESCE(sum(c.gaps_total),0)::bigint FROM olp.export_sinks s LEFT JOIN olp.export_cursors c ON c.sink_id=s.id
 GROUP BY s.id ORDER BY s.id LIMIT 1001`
const exportPendingMetricsSQL = `SELECT count(*) FROM olp.export_pending WHERE delivered_at IS NULL`
