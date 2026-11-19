ALTER TABLE olp.export_pending ADD COLUMN rejected_at timestamptz;
DROP INDEX olp.export_pending_due;
CREATE INDEX export_pending_due ON olp.export_pending(sink_id,next_attempt_at,record_id)
 WHERE delivered_at IS NULL AND rejected_at IS NULL;

CREATE OR REPLACE FUNCTION olp.maintain_exports(p_limit integer) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE v_deleted bigint;
BEGIN
 WITH candidates AS MATERIALIZED (
  SELECT r.id,r.stream,r.occurred_at,
   r.occurred_at < now()-make_interval(days=>COALESCE((SELECT s.value::integer FROM olp.settings s WHERE s.key=
    CASE r.stream WHEN 'audit' THEN 'retention.audit_days' WHEN 'usage_rollups' THEN 'retention.usage_days' ELSE 'retention.requests_days' END),
    CASE r.stream WHEN 'audit' THEN 365 WHEN 'usage_rollups' THEN 90 ELSE 30 END)) AS expired
  FROM olp.export_records r
  WHERE NOT EXISTS(SELECT 1 FROM olp.export_pending p WHERE p.record_id=r.id AND p.delivered_at IS NULL AND p.rejected_at IS NULL)
   OR r.occurred_at < now()-make_interval(days=>COALESCE((SELECT s.value::integer FROM olp.settings s WHERE s.key=
    CASE r.stream WHEN 'audit' THEN 'retention.audit_days' WHEN 'usage_rollups' THEN 'retention.usage_days' ELSE 'retention.requests_days' END),
    CASE r.stream WHEN 'audit' THEN 365 WHEN 'usage_rollups' THEN 90 ELSE 30 END))
  ORDER BY r.occurred_at,r.id LIMIT p_limit FOR UPDATE OF r SKIP LOCKED
 ), gaps AS (
  INSERT INTO olp.export_gaps(sink_id,stream,record_count,first_occurred_at,last_occurred_at,reason)
  SELECT p.sink_id,c.stream,count(*),min(c.occurred_at),max(c.occurred_at),'retention_expired'
  FROM candidates c JOIN olp.export_pending p ON p.record_id=c.id
  WHERE c.expired AND p.delivered_at IS NULL AND p.rejected_at IS NULL GROUP BY p.sink_id,c.stream
  RETURNING sink_id,stream,record_count
 ), checkpointed_gaps AS (
  INSERT INTO olp.export_cursors(sink_id,stream,gaps_total)
  SELECT sink_id,stream,sum(record_count) FROM gaps GROUP BY sink_id,stream
  ON CONFLICT(sink_id,stream) DO UPDATE SET gaps_total=export_cursors.gaps_total+EXCLUDED.gaps_total,updated_at=now()
  RETURNING sink_id
 ), removed AS (
  DELETE FROM olp.export_records r USING candidates c WHERE r.id=c.id AND (SELECT count(*) FROM checkpointed_gaps)>=0 RETURNING r.id
 ) SELECT count(*) INTO v_deleted FROM removed;
 DELETE FROM olp.export_gaps WHERE recorded_at<now()-make_interval(days=>COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.audit_days'),365));
 RETURN v_deleted;
END $$;
