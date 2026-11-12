package stream

const (
	maintainSQL    = `SELECT olp.maintain_exports($1)`
	activePairsSQL = `SELECT s.id::text,st.stream FROM (SELECT id,streams FROM olp.export_sinks WHERE enabled ORDER BY id LIMIT 1001) s
 CROSS JOIN LATERAL unnest(s.streams) WITH ORDINALITY st(stream,n) ORDER BY s.id,st.n`
	lockStreamSQL = `SELECT pg_try_advisory_xact_lock(hashtextextended($1,501))`
	readSinkSQL   = `SELECT type,format,destination,settings,credential_id::text,created_by::text
 FROM olp.export_sinks WHERE id=$1 AND enabled AND $2=ANY(streams) FOR SHARE`
	nextRecordSQL = `SELECT r.id::text,r.occurred_at,
 jsonb_build_object('version',1,'event_id',r.id,'stream',r.stream,'source_id',r.source_id,'project_id',r.project_id,
 'route',r.route,'outcome',r.outcome,'occurred_at',r.occurred_at,'data',r.payload),p.attempts,r.source_id::text,r.project_id::text
 FROM olp.export_pending p JOIN olp.export_records r ON r.id=p.record_id
 WHERE p.sink_id=$1 AND r.stream=$2 AND p.delivered_at IS NULL AND p.rejected_at IS NULL AND p.next_attempt_at<=now()
 ORDER BY r.queued_at,r.id LIMIT 1`
	claimRecordSQL = `UPDATE olp.export_pending SET attempts=LEAST(attempts+1,2147483647),
 last_attempt_at=now(),next_attempt_at=now()+interval '2 minutes' WHERE sink_id=$1 AND record_id=$2 AND delivered_at IS NULL AND rejected_at IS NULL AND attempts=$3`
	rejectEvidenceSQL = `UPDATE olp.export_pending SET rejected_at=now(),last_error_code='evidence_unavailable' WHERE sink_id=$1 AND record_id=$2 AND delivered_at IS NULL AND rejected_at IS NULL`
	finishRecordSQL   = `UPDATE olp.export_pending SET delivered_at=CASE WHEN $3 THEN now() ELSE NULL END,
 rejected_at=CASE WHEN $6 THEN now() ELSE NULL END,
 last_error_code=CASE WHEN $3 THEN NULL ELSE $4 END,next_attempt_at=now()+make_interval(secs=>$5)
 WHERE sink_id=$1 AND record_id=$2 AND delivered_at IS NULL AND rejected_at IS NULL`
	checkpointCursorSQL = `INSERT INTO olp.export_cursors(sink_id,stream,cursor,delivered_total,failed_total,last_success_at)
 VALUES($1,$2,$3,$4,$5,CASE WHEN $4::bigint>0 THEN now() END) ON CONFLICT(sink_id,stream) DO UPDATE SET
 cursor=COALESCE(EXCLUDED.cursor,export_cursors.cursor),
 delivered_total=export_cursors.delivered_total+EXCLUDED.delivered_total,
 failed_total=export_cursors.failed_total+EXCLUDED.failed_total,last_success_at=COALESCE(EXCLUDED.last_success_at,export_cursors.last_success_at),updated_at=now()`
	lagMetricsSQL = `SELECT s.id::text,st.stream,COALESCE(EXTRACT(epoch FROM now()-min(r.queued_at)),0)::double precision
 FROM olp.export_sinks s CROSS JOIN LATERAL unnest(s.streams) AS st(stream)
 LEFT JOIN olp.export_pending p ON p.sink_id=s.id AND p.delivered_at IS NULL AND p.rejected_at IS NULL
 LEFT JOIN olp.export_records r ON r.id=p.record_id AND r.stream=st.stream
 WHERE s.enabled GROUP BY s.id,st.stream ORDER BY s.id,st.stream LIMIT 5001`
	deliveryMetricsSQL = `SELECT s.id::text,COALESCE(sum(c.delivered_total),0)::bigint,COALESCE(sum(c.failed_total),0)::bigint,
 COALESCE(sum(c.gaps_total),0)::bigint FROM olp.export_sinks s LEFT JOIN olp.export_cursors c ON c.sink_id=s.id
 GROUP BY s.id ORDER BY s.id LIMIT 1001`
	pendingMetricsSQL = `SELECT count(*) FROM olp.export_pending WHERE delivered_at IS NULL AND rejected_at IS NULL`
)
