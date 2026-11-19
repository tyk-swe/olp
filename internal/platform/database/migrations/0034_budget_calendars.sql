-- Active history is immutable. A requested zone replaces only future changes;
-- each period finishes its existing window before adopting the new calendar.
CREATE TABLE olp.budget_calendar_changes (
    window_kind text NOT NULL CHECK (window_kind IN ('day','week','month')),
    effective_at timestamptz NOT NULL,
    time_zone text NOT NULL,
    PRIMARY KEY (window_kind,effective_at)
);
INSERT INTO olp.budget_calendar_changes
SELECT kind,'1970-01-01 00:00:00+00','UTC' FROM unnest(ARRAY['day','week','month']) kind;
INSERT INTO olp.settings(key,value,etag,updated_by)
 SELECT 'budgets.time_zone','UTC',uuidv7(),id FROM olp.users WHERE role='owner' ORDER BY id LIMIT 1;

-- PostgreSQL's AT TIME ZONE chooses the later repeated midnight. Find the
-- first instant of the civil date instead, including skipped dates.
CREATE FUNCTION olp.budget_civil_boundary(civil_date date, zone_name text)
RETURNS timestamptz LANGUAGE plpgsql STABLE STRICT AS $$
DECLARE low bigint; high bigint; middle bigint;
BEGIN
 IF zone_name='UTC' THEN RETURN civil_date::timestamp AT TIME ZONE 'UTC'; END IF;
 low:=extract(epoch FROM civil_date::timestamp AT TIME ZONE 'UTC')::bigint-172800;
 high:=low+345600;
 WHILE low<high LOOP
  middle:=low+(high-low)/2;
  IF (to_timestamp(middle) AT TIME ZONE zone_name)::date<civil_date THEN low:=middle+1;
  ELSE high:=middle; END IF;
 END LOOP;
 RETURN to_timestamp(low);
END $$;

CREATE FUNCTION olp.budget_window(kind text, instant timestamptz)
RETURNS TABLE(window_id bigint,start_at timestamptz,end_at timestamptz,time_zone text)
LANGUAGE plpgsql STABLE STRICT AS $$
DECLARE change_at timestamptz; next_change timestamptz; civil_date date; next_date date;
BEGIN
 IF kind NOT IN ('day','week','month') THEN RAISE EXCEPTION 'invalid budget window'; END IF;
 SELECT c.effective_at,c.time_zone INTO STRICT change_at,time_zone
 FROM olp.budget_calendar_changes c WHERE c.window_kind=kind AND c.effective_at<=instant
 ORDER BY c.effective_at DESC LIMIT 1;
 SELECT min(c.effective_at) INTO next_change FROM olp.budget_calendar_changes c
 WHERE c.window_kind=kind AND c.effective_at>instant;
 civil_date:=date_trunc(kind,instant AT TIME ZONE time_zone)::date;
 next_date:=CASE kind WHEN 'day' THEN civil_date+1 WHEN 'week' THEN civil_date+7 ELSE (civil_date+interval '1 month')::date END;
 start_at:=GREATEST(olp.budget_civil_boundary(civil_date,time_zone),change_at);
 end_at:=LEAST(olp.budget_civil_boundary(next_date,time_zone),next_change);
 -- Preserve pre-feature UTC identifiers. Subsequent windows use their exact
 -- start instant, which is monotone and cannot collide with legacy labels.
 IF change_at='1970-01-01 00:00:00+00'::timestamptz THEN
  window_id:=CASE kind WHEN 'day' THEN floor(extract(epoch FROM start_at)/86400)::bigint
   WHEN 'week' THEN floor((floor(extract(epoch FROM start_at)/86400)+3)/7)::bigint
   ELSE extract(year FROM civil_date)::bigint*12+extract(month FROM civil_date)::bigint-1 END;
 ELSE window_id:=extract(epoch FROM start_at)::bigint; END IF;
 RETURN NEXT;
END $$;

CREATE FUNCTION olp.budget_windows(instant timestamptz)
RETURNS TABLE(daily_id bigint,daily_start timestamptz,daily_end timestamptz,
 monthly_id bigint,monthly_start timestamptz,monthly_end timestamptz,
 weekly_id bigint,weekly_start timestamptz,weekly_end timestamptz)
LANGUAGE sql STABLE STRICT AS $$
 SELECT d.window_id,d.start_at,d.end_at,m.window_id,m.start_at,m.end_at,w.window_id,w.start_at,w.end_at
 FROM olp.budget_window('day',instant) d CROSS JOIN olp.budget_window('month',instant) m CROSS JOIN olp.budget_window('week',instant) w
$$;

CREATE FUNCTION olp.schedule_budget_zone(zone_name text, instant timestamptz)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE kind text; current_window record;
BEGIN
 -- Validate against the database's installed IANA rules as well as Go's rules.
 PERFORM instant AT TIME ZONE zone_name;
 FOREACH kind IN ARRAY ARRAY['day','week','month'] LOOP
  DELETE FROM olp.budget_calendar_changes WHERE window_kind=kind AND effective_at>instant;
  SELECT * INTO STRICT current_window FROM olp.budget_window(kind,instant);
  IF current_window.time_zone<>zone_name THEN
   INSERT INTO olp.budget_calendar_changes VALUES(kind,current_window.end_at,zone_name);
  END IF;
 END LOOP;
END $$;

-- Keep ordinary report buckets hourly while splitting cost evidence wherever
-- a calendar boundary falls inside an hour (e.g. Kathmandu or Lord Howe).
CREATE FUNCTION olp.budget_bucket(instant timestamptz) RETURNS timestamptz
LANGUAGE sql STABLE STRICT AS $$
 SELECT GREATEST(date_trunc('hour',instant AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',daily_start,weekly_start,monthly_start)
 FROM olp.budget_windows(instant)
$$;
ALTER TABLE olp.attempt_usage_hourly ADD COLUMN budget_bucket timestamptz;
UPDATE olp.attempt_usage_hourly SET budget_bucket=bucket;
ALTER TABLE olp.attempt_usage_hourly ALTER COLUMN budget_bucket SET NOT NULL,
 DROP CONSTRAINT attempt_usage_hourly_dimensions_key,
 ADD CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
 (bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,budget_group_id,
 attribution,model_family,estimate_provenance,end_user_digest,budget_bucket);
-- Retained fixture/import rows with no split use their existing hourly bucket.
CREATE FUNCTION olp.default_budget_bucket() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN NEW.budget_bucket:=COALESCE(NEW.budget_bucket,NEW.bucket); RETURN NEW; END $$;
CREATE TRIGGER default_budget_bucket BEFORE INSERT ON olp.attempt_usage_hourly
 FOR EACH ROW EXECUTE FUNCTION olp.default_budget_bucket();

CREATE FUNCTION olp.budget_calendar_status(instant timestamptz) RETURNS jsonb LANGUAGE sql STABLE AS $$
 SELECT jsonb_agg(jsonb_build_object('window_kind',k.kind,'time_zone',w.time_zone,'starts_at',w.start_at,'ends_at',w.end_at,
 'pending_time_zone',c.time_zone,'effective_at',c.effective_at) ORDER BY k.ordinal)
 FROM unnest(ARRAY['day','week','month']) WITH ORDINALITY k(kind,ordinal)
 CROSS JOIN LATERAL olp.budget_window(k.kind,instant) w
 LEFT JOIN LATERAL (SELECT time_zone,effective_at FROM olp.budget_calendar_changes WHERE window_kind=k.kind AND effective_at>instant ORDER BY effective_at LIMIT 1) c ON true
$$;
