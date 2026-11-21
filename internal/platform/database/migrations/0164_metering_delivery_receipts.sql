ALTER TABLE olp.export_pending ADD COLUMN send_may_have_occurred boolean NOT NULL DEFAULT false;
UPDATE olp.export_pending SET send_may_have_occurred=true WHERE last_attempt_at IS NOT NULL;

CREATE TABLE olp.metering_delivery_receipts (
 sink_id uuid NOT NULL REFERENCES olp.export_sinks ON DELETE CASCADE,
 attempt_id uuid NOT NULL,
 record_id uuid NOT NULL,
 project_id uuid,
 outcome text NOT NULL CHECK(outcome IN ('accepted','delivery_unconfirmed','expired_before_delivery')),
 last_error_code text,
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 occurred_at timestamptz NOT NULL,
 queued_at timestamptz NOT NULL,
 last_attempt_at timestamptz,
 settled_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(sink_id,attempt_id)
);
CREATE INDEX metering_delivery_receipts_settled ON olp.metering_delivery_receipts(settled_at,sink_id,attempt_id);

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
 ), locked_pending AS MATERIALIZED (
  SELECT p.sink_id,p.record_id,p.attempts,p.last_attempt_at,p.last_error_code,p.delivered_at,p.rejected_at,p.send_may_have_occurred
  FROM olp.export_pending p JOIN candidates c ON c.id=p.record_id FOR UPDATE OF p
 ), receipts AS (
  INSERT INTO olp.metering_delivery_receipts(sink_id,attempt_id,record_id,project_id,outcome,last_error_code,attempts,occurred_at,queued_at,last_attempt_at,settled_at)
  SELECT lp.sink_id,r.source_id,r.id,r.project_id,
   CASE WHEN lp.delivered_at IS NOT NULL THEN 'accepted'
        WHEN lp.send_may_have_occurred THEN 'delivery_unconfirmed'
        ELSE 'expired_before_delivery' END,
   CASE WHEN lp.delivered_at IS NOT NULL THEN NULL ELSE lp.last_error_code END,
   lp.attempts,r.occurred_at,r.queued_at,lp.last_attempt_at,COALESCE(lp.delivered_at,lp.rejected_at,now())
  FROM candidates c
  JOIN olp.export_records r ON r.id=c.id
  JOIN locked_pending lp ON lp.record_id=r.id
  JOIN olp.export_sinks s ON s.id=lp.sink_id
  WHERE r.stream='metering' AND s.format='cloudevents'
  ON CONFLICT(sink_id,attempt_id) DO UPDATE SET
   record_id=EXCLUDED.record_id,project_id=EXCLUDED.project_id,outcome=EXCLUDED.outcome,
   last_error_code=EXCLUDED.last_error_code,attempts=EXCLUDED.attempts,
   occurred_at=EXCLUDED.occurred_at,queued_at=EXCLUDED.queued_at,
   last_attempt_at=EXCLUDED.last_attempt_at,settled_at=EXCLUDED.settled_at
  WHERE metering_delivery_receipts.outcome<>'accepted'
  RETURNING sink_id
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
  DELETE FROM olp.export_records r USING candidates c WHERE r.id=c.id AND (SELECT count(*) FROM checkpointed_gaps)>=0 AND (SELECT count(*) FROM receipts)>=0 RETURNING r.id
 ) SELECT count(*) INTO v_deleted FROM removed;
 DELETE FROM olp.export_gaps WHERE recorded_at<now()-make_interval(days=>COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.audit_days'),365));
 DELETE FROM olp.metering_delivery_receipts WHERE settled_at<now()-make_interval(days=>COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.requests_days'),30));
 RETURN v_deleted;
END $$;
