CREATE FUNCTION olp.dirty_commitment_observed_days() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE first_day timestamptz; after_last_day timestamptz;
BEGIN
 SELECT date_trunc('day',bucket_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
  (date_trunc('day',(bucket_end-interval '1 microsecond') AT TIME ZONE 'UTC')+interval '1 day') AT TIME ZONE 'UTC'
 INTO first_day,after_last_day FROM olp.commitment_periods WHERE id=NEW.period_id;
 IF NOT EXISTS(SELECT 1 FROM olp.commitment_usage WHERE period_id=NEW.period_id AND observed_at<first_day)
  AND NOT EXISTS(SELECT 1 FROM olp.commitment_usage WHERE period_id=NEW.period_id AND observed_at>=after_last_day)
 THEN RETURN NEW; END IF;
 INSERT INTO olp.focus_dirty_days(day,version)
 SELECT affected.day,nextval('olp.focus_dirty_version') FROM (
  SELECT day::date FROM generate_series(first_day AT TIME ZONE 'UTC',
   (after_last_day-interval '1 microsecond') AT TIME ZONE 'UTC',interval '1 day') day
  UNION
  SELECT (observed_at AT TIME ZONE 'UTC')::date FROM olp.commitment_usage
   WHERE period_id=NEW.period_id AND observed_at<first_day
  UNION
  SELECT (observed_at AT TIME ZONE 'UTC')::date FROM olp.commitment_usage
   WHERE period_id=NEW.period_id AND observed_at>=after_last_day
 ) affected ORDER BY affected.day
 ON CONFLICT(day) DO UPDATE SET version=EXCLUDED.version;
 RETURN NEW;
END $$;
CREATE TRIGGER dirty_commitment_observed_days AFTER INSERT OR UPDATE OF weight,eligible,complete,payload
 ON olp.commitment_usage FOR EACH ROW EXECUTE FUNCTION olp.dirty_commitment_observed_days();

INSERT INTO olp.focus_dirty_days(day,version)
SELECT affected.day,nextval('olp.focus_dirty_version') FROM (
 SELECT DISTINCT (u.observed_at AT TIME ZONE 'UTC')::date AS day
 FROM olp.commitment_usage u JOIN olp.commitment_periods p ON p.id=u.period_id
 WHERE (u.observed_at AT TIME ZONE 'UTC')::date < (p.bucket_start AT TIME ZONE 'UTC')::date
  OR (u.observed_at AT TIME ZONE 'UTC')::date > ((p.bucket_end-interval '1 microsecond') AT TIME ZONE 'UTC')::date
) affected ORDER BY affected.day
ON CONFLICT(day) DO UPDATE SET version=EXCLUDED.version;
