-- Commitments are obligations of an installation, not a side effect of traffic.
-- Identity and display provenance deliberately have no cascading foreign keys.
CREATE TABLE olp.commitment_schedules (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), provider_id uuid NOT NULL,
 provider_revision_id uuid NOT NULL, provider_name text NOT NULL, provider_kind text NOT NULL,
 vendor_id text, project_id uuid, project_name text,
 amount numeric(24,12) NOT NULL CHECK(amount>0), currency text NOT NULL,
 period text NOT NULL CHECK(period IN ('hour','day','month')),
 effective_at timestamptz NOT NULL, ends_at timestamptz,
 materialized_through timestamptz,
 historical boolean NOT NULL DEFAULT false,
 CHECK(ends_at IS NULL OR ends_at>effective_at), UNIQUE(provider_id,effective_at)
);
CREATE INDEX commitment_schedules_provider ON olp.commitment_schedules(provider_id,effective_at DESC);
CREATE TABLE olp.commitment_periods (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), schedule_id uuid NOT NULL REFERENCES olp.commitment_schedules,
 provider_id uuid NOT NULL, period text NOT NULL, legacy boolean NOT NULL DEFAULT false,
 bucket_start timestamptz NOT NULL, bucket_end timestamptz NOT NULL,
 amount numeric(24,12) NOT NULL CHECK(amount>0), complete boolean NOT NULL DEFAULT true,
 UNIQUE(schedule_id,bucket_start), UNIQUE(provider_id,period,bucket_start), CHECK(bucket_end>bucket_start)
);
CREATE FUNCTION olp.commitment_period_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 SELECT provider_id,period INTO NEW.provider_id,NEW.period FROM olp.commitment_schedules WHERE id=NEW.schedule_id;
 RETURN NEW;
END $$;
CREATE TRIGGER commitment_period_identity BEFORE INSERT ON olp.commitment_periods
 FOR EACH ROW EXECUTE FUNCTION olp.commitment_period_identity();
CREATE INDEX commitment_periods_date ON olp.commitment_periods(bucket_start);
-- Compact accounting evidence survives request/fact retention for the whole
-- period. No prompts, completions, credentials or raw request bodies are kept.
CREATE TABLE olp.commitment_usage (
 attempt_id uuid PRIMARY KEY, period_id uuid NOT NULL REFERENCES olp.commitment_periods ON DELETE CASCADE,
 observed_at timestamptz NOT NULL, weight numeric(24,12) NOT NULL DEFAULT 0,
 eligible boolean NOT NULL, complete boolean NOT NULL, payload jsonb NOT NULL
);
CREATE INDEX commitment_usage_observed ON olp.commitment_usage(observed_at);
CREATE INDEX commitment_usage_period ON olp.commitment_usage(period_id,observed_at,attempt_id);
ALTER TABLE olp.attempt_usage_facts ADD COLUMN commitment_schedule_id uuid,
 ADD COLUMN commitment_bucket timestamptz;
ALTER TABLE olp.focus_datasets ADD COLUMN commitment_totals jsonb;
ALTER TABLE olp.attempt_usage_hourly ADD COLUMN commitment_backfill_pending boolean NOT NULL DEFAULT true;
ALTER TABLE olp.attempt_usage_hourly ALTER COLUMN commitment_backfill_pending SET DEFAULT false;
CREATE INDEX attempt_usage_hourly_commitment_backfill ON olp.attempt_usage_hourly(bucket)
 WHERE commitment_backfill_pending AND commitment_period IS NOT NULL;
CREATE TABLE olp.commitment_cutover (singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton), at timestamptz NOT NULL);
INSERT INTO olp.commitment_cutover(at) VALUES(now());

CREATE FUNCTION olp.commitment_bucket(p_period text,p_at timestamptz) RETURNS timestamptz
LANGUAGE sql IMMUTABLE STRICT AS $$ SELECT date_trunc(p_period,p_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' $$;
CREATE FUNCTION olp.commitment_end(p_period text,p_at timestamptz) RETURNS timestamptz
LANGUAGE sql IMMUTABLE STRICT AS $$ SELECT ((p_at AT TIME ZONE 'UTC')+('1 '||p_period)::interval) AT TIME ZONE 'UTC' $$;

CREATE FUNCTION olp.dirty_commitment(p_start timestamptz,p_end timestamptz) RETURNS void LANGUAGE sql AS $$
 INSERT INTO olp.focus_dirty_days(day,version)
 SELECT day::date,nextval('olp.focus_dirty_version') FROM generate_series(
  p_start AT TIME ZONE 'UTC', (p_end-interval '1 microsecond') AT TIME ZONE 'UTC',interval '1 day') day
 ON CONFLICT(day) DO UPDATE SET version=EXCLUDED.version
$$;
CREATE FUNCTION olp.commitment_period_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM olp.dirty_commitment(NEW.bucket_start,NEW.bucket_end); RETURN NEW; END $$;
CREATE TRIGGER commitment_period_dirty AFTER INSERT ON olp.commitment_periods
 FOR EACH ROW EXECUTE FUNCTION olp.commitment_period_dirty();

-- Publication holds the installation lock. Replace future intent, never an
-- incurred period. A changed cadence starts at the next common UTC boundary.
CREATE FUNCTION olp.publish_commitments(p_at timestamptz) RETURNS void LANGUAGE plpgsql AS $$
DECLARE d record; s olp.commitment_schedules; boundary timestamptz; cadence text;
BEGIN
 FOR d IN
  SELECT p.id, r.id revision_id, r.name, p.kind, r.configuration::jsonb cfg,
   p.project_id, pj.name project_name,
   r.configuration::jsonb#>'{options,commitment}' desired
  FROM olp.providers p JOIN olp.provider_revisions r ON r.id=p.active_revision_id
  LEFT JOIN olp.projects pj ON pj.id=p.project_id WHERE p.state IN ('active','disabled')
  UNION ALL
  SELECT DISTINCT cs.provider_id,NULL::uuid,NULL,NULL,NULL::jsonb,NULL::uuid,NULL,NULL::jsonb
  FROM olp.commitment_schedules cs WHERE NOT EXISTS (
   SELECT 1 FROM olp.providers p WHERE p.id=cs.provider_id AND p.active_revision_id IS NOT NULL AND p.state IN ('active','disabled'))
 LOOP
  SELECT * INTO s FROM olp.commitment_schedules WHERE provider_id=d.id AND effective_at<=p_at
   AND (ends_at IS NULL OR ends_at>p_at) ORDER BY effective_at DESC LIMIT 1 FOR UPDATE;
  IF FOUND AND d.desired->>'amount' IS NOT NULL
   AND s.amount=(d.desired->>'amount')::numeric AND s.period=d.desired->>'period'
   AND s.provider_name=d.name AND s.project_id IS NOT DISTINCT FROM d.project_id
   AND s.vendor_id IS NOT DISTINCT FROM d.cfg#>>'{options,vendor_id}' THEN
   DELETE FROM olp.commitment_schedules WHERE provider_id=d.id AND effective_at>p_at;
   UPDATE olp.commitment_schedules SET ends_at=NULL WHERE id=s.id;
   CONTINUE;
  END IF;
  DELETE FROM olp.commitment_schedules WHERE provider_id=d.id AND effective_at>p_at;
  boundary:=p_at;
  IF s.id IS NOT NULL THEN
   cadence:=s.period;
   IF d.desired->>'period'='month' OR s.period='month' THEN cadence:='month';
   ELSIF d.desired->>'period'='day' OR s.period='day' THEN cadence:='day'; END IF;
   boundary:=olp.commitment_end(cadence,olp.commitment_bucket(cadence,p_at));
   UPDATE olp.commitment_schedules SET ends_at=boundary WHERE id=s.id;
  END IF;
  IF d.desired->>'amount' IS NOT NULL THEN
   INSERT INTO olp.commitment_schedules(provider_id,provider_revision_id,provider_name,provider_kind,vendor_id,
    project_id,project_name,amount,currency,period,effective_at)
   VALUES(d.id,d.revision_id,d.name,d.kind,d.cfg#>>'{options,vendor_id}',d.project_id,d.project_name,
    (d.desired->>'amount')::numeric,COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD'),
    d.desired->>'period',boundary);
  END IF;
 END LOOP;
END $$;

-- Recover only published history, including the preceding release needed at
-- the retention boundary. Never read unpublished provider configuration.
WITH retained AS (
 SELECT * FROM olp.runtime_releases WHERE published_at>=now()-make_interval(days=>COALESCE(
  (SELECT value::integer FROM olp.settings WHERE key='retention.usage_days'),90))
 OR sequence=(SELECT max(sequence) FROM olp.runtime_releases WHERE published_at<now()-make_interval(days=>COALESCE(
  (SELECT value::integer FROM olp.settings WHERE key='retention.usage_days'),90)))
), identities AS (
 SELECT DISTINCT e.key FROM retained r CROSS JOIN LATERAL jsonb_each(r.snapshot::jsonb->'providers') e
), history AS (
 SELECT r.published_at,r.sequence,i.key::uuid provider_id,r.snapshot::jsonb->'providers'->i.key p
 FROM retained r CROSS JOIN identities i
), published AS (
 SELECT *,lag(p->'commitment') OVER(PARTITION BY provider_id ORDER BY sequence) previous
 FROM history
), changes AS (
 SELECT *,lead(published_at) OVER(PARTITION BY provider_id ORDER BY sequence) next_at
 FROM published WHERE p->'commitment' IS DISTINCT FROM previous
)
INSERT INTO olp.commitment_schedules(provider_id,provider_revision_id,provider_name,provider_kind,vendor_id,
 project_id,project_name,amount,currency,period,effective_at,ends_at,historical)
SELECT provider_id,(p->>'revision_id')::uuid,p->>'name',p->>'kind',p->>'vendor_id',
 NULLIF(p->>'project_id','')::uuid,(SELECT name FROM olp.projects WHERE id=NULLIF(p->>'project_id','')::uuid),
 (p#>>'{commitment,amount}')::numeric,COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD'),
 p#>>'{commitment,period}',published_at,next_at,true FROM changes
WHERE p#>>'{commitment,amount}' IS NOT NULL;

-- A period is inserted once, including when settlement precedes a worker pass.
CREATE FUNCTION olp.ensure_commitment_period(p_schedule uuid,p_bucket timestamptz) RETURNS uuid LANGUAGE plpgsql AS $$
DECLARE result uuid;
BEGIN
 INSERT INTO olp.commitment_periods(schedule_id,bucket_start,bucket_end,amount,complete,legacy)
 SELECT id,p_bucket,olp.commitment_end(period,p_bucket),
 CASE WHEN historical AND p_bucket<(SELECT at FROM olp.commitment_cutover) THEN COALESCE((
 SELECT e.amount FROM (
  SELECT f.commitment_amount amount,f.observed_at at FROM olp.attempt_usage_facts f
  WHERE f.provider_id=cs.provider_id AND f.commitment_period=cs.period AND f.observed_at>=p_bucket AND f.observed_at<olp.commitment_end(cs.period,p_bucket)
  UNION ALL SELECT h.commitment_amount,h.bucket FROM olp.attempt_usage_hourly h
  WHERE h.provider_id=cs.provider_id AND h.commitment_period=cs.period AND h.bucket>=p_bucket AND h.bucket<olp.commitment_end(cs.period,p_bucket)
 ) e WHERE e.amount IS NOT NULL ORDER BY e.at DESC,e.amount DESC LIMIT 1),amount) ELSE amount END,
  NOT (historical AND p_bucket < (SELECT at FROM olp.commitment_cutover)),
  historical AND p_bucket < (SELECT at FROM olp.commitment_cutover)
 FROM olp.commitment_schedules cs WHERE id=p_schedule
 ON CONFLICT(provider_id,period,bucket_start) DO NOTHING;
 SELECT p.id INTO result FROM olp.commitment_periods p JOIN olp.commitment_schedules s ON s.id=p_schedule
 WHERE p.provider_id=s.provider_id AND p.period=s.period AND p.bucket_start=p_bucket;
 RETURN result;
END $$;

-- Retain only accounting/export dimensions and price evidence. Request/event
-- identities and raw metering counters follow their normal fact retention.
CREATE FUNCTION olp.commitment_fact_payload(f olp.attempt_usage_facts) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $$ SELECT jsonb_build_object(
 'attempt_id',(f).attempt_id,
 'provider_id',(f).provider_id,
 'route_slug',(f).route_slug,
 'api_key_id',(f).api_key_id,
 'upstream_model',(f).upstream_model,
 'operation',(f).operation,
 'surface',(f).surface,
 'attribution',(f).attribution,
 'budget_group_id',(f).budget_group_id,
 'end_user_digest',(f).end_user_digest,
 'model_family',(f).model_family,
 'estimate_provenance',(f).estimate_provenance,
 'budget_exempt',(f).budget_exempt,
 'observed_at',(f).observed_at,
 'commitment_weight',(f).commitment_weight,
 'commitment_amount',(f).commitment_amount,
 'commitment_period',(f).commitment_period,
 'commitment_schedule_id',(f).commitment_schedule_id,
 'commitment_bucket',(f).commitment_bucket,
 'list_cost',(f).list_cost,
 'contracted_cost',(f).contracted_cost,
 'estimated_cost',(f).estimated_cost,
 'currency',(f).currency,
 'charge',(f).charge,
 'rate_card_revision_id',(f).rate_card_revision_id,
 'discount_id',(f).discount_id,
 'pricing_revision_id',(f).pricing_revision_id,
 'usage_complete',(f).usage_complete,
 'charge_status',(f).charge_status,
 'unpriced',(f).unpriced,
 'caller_credential',(f).caller_credential,
 'cost_lines',(f).cost_lines) $$;

CREATE FUNCTION olp.capture_commitment_usage() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s olp.commitment_schedules; pid uuid; started timestamptz; consumer uuid; cname text; doc jsonb;
BEGIN
 SELECT COALESCE(a.started_at,NEW.observed_at) INTO started FROM olp.attempts a WHERE a.id=NEW.attempt_id;
 started:=COALESCE(started,NEW.observed_at);
 SELECT * INTO s FROM olp.commitment_schedules WHERE id=NEW.commitment_schedule_id;
 IF s.id IS NULL THEN
  SELECT * INTO s FROM olp.commitment_schedules WHERE provider_id=NEW.provider_id AND effective_at<=started
   AND (ends_at IS NULL OR ends_at>started) ORDER BY effective_at DESC LIMIT 1;
 END IF;
 IF s.id IS NULL THEN RETURN NEW; END IF;
 NEW.commitment_schedule_id:=s.id;
 NEW.commitment_bucket:=COALESCE(NEW.commitment_bucket,olp.commitment_bucket(s.period,started));
 pid:=olp.ensure_commitment_period(s.id,NEW.commitment_bucket);
 SELECT COALESCE(k.project_id,s.project_id,r.project_id) INTO consumer
 FROM (SELECT 1) x LEFT JOIN olp.api_keys k ON k.id=NEW.api_key_id LEFT JOIN olp.routes r ON r.slug=NEW.route_slug;
 SELECT name INTO cname FROM olp.projects WHERE id=consumer;
 doc:=olp.commitment_fact_payload(NEW)||jsonb_build_object('consumer',consumer,'consumer_name',cname,
  'connection_name',s.provider_name,'provider_kind',s.provider_kind,'vendor_id',s.vendor_id,'provider_project',s.project_id);
 INSERT INTO olp.commitment_usage(attempt_id,period_id,observed_at,weight,eligible,complete,payload)
 VALUES(NEW.attempt_id,pid,NEW.observed_at,COALESCE(NEW.commitment_weight,0),
  NOT NEW.caller_credential AND NOT NEW.budget_exempt AND NEW.charge_status='billable' AND NEW.usage_complete AND NEW.estimated_cost IS NOT NULL,
  NEW.charge_status='not_billable' OR (NEW.usage_complete AND NOT NEW.unpriced),doc)
 ON CONFLICT(attempt_id) DO UPDATE SET weight=EXCLUDED.weight,eligible=EXCLUDED.eligible,complete=EXCLUDED.complete,payload=EXCLUDED.payload;
 PERFORM olp.dirty_commitment(NEW.commitment_bucket,olp.commitment_end(s.period,NEW.commitment_bucket));
 RETURN NEW;
END $$;
CREATE TRIGGER capture_commitment_usage BEFORE INSERT OR UPDATE OF estimated_cost,commitment_weight,usage_complete
 ON olp.attempt_usage_facts FOR EACH ROW EXECUTE FUNCTION olp.capture_commitment_usage();

-- Cumulative differences place every residual deterministically. The final
-- positive-weight row receives the remainder, so no money can disappear.
CREATE VIEW olp.commitment_allocations AS
 SELECT u.*, CASE WHEN total_weight>0 THEN
  round(amount*running_weight/total_weight,12)-round(amount*(running_weight-weight)/total_weight,12)
  ELSE 0 END AS allocated_cost
 FROM (
  SELECT u.*,p.amount,
   sum(weight) OVER(PARTITION BY period_id) total_weight,
   sum(weight) OVER(PARTITION BY period_id ORDER BY observed_at,attempt_id ROWS UNBOUNDED PRECEDING) running_weight
  FROM olp.commitment_usage u JOIN olp.commitment_periods p ON p.id=u.period_id WHERE eligible
 ) u;

CREATE FUNCTION olp.materialize_commitments(p_now timestamptz,p_cutoff timestamptz,p_limit integer DEFAULT 512)
RETURNS integer LANGUAGE plpgsql AS $$
DECLARE s olp.commitment_schedules; b timestamptz; n integer:=0;
BEGIN
 FOR s IN SELECT * FROM olp.commitment_schedules
  WHERE effective_at<=p_now AND COALESCE(materialized_through,effective_at)<=p_now
   AND (ends_at IS NULL OR COALESCE(materialized_through,effective_at)<ends_at)
  ORDER BY COALESCE(materialized_through,effective_at),id FOR UPDATE SKIP LOCKED
 LOOP
  b:=GREATEST(COALESCE(s.materialized_through,s.effective_at),olp.commitment_bucket(s.period,GREATEST(s.effective_at,p_cutoff)));
  b:=olp.commitment_bucket(s.period,b);
  WHILE b<=p_now AND (s.ends_at IS NULL OR b<s.ends_at) AND n<p_limit LOOP
   PERFORM olp.ensure_commitment_period(s.id,b);
   b:=olp.commitment_end(s.period,b); n:=n+1;
  END LOOP;
  UPDATE olp.commitment_schedules SET materialized_through=b WHERE id=s.id;
  EXIT WHEN n>=p_limit;
 END LOOP;
 RETURN n;
END $$;

CREATE FUNCTION olp.commitment_hourly_allocation(h olp.attempt_usage_hourly) RETURNS numeric
LANGUAGE sql STABLE AS $$
 SELECT SUM(COALESCE(a.allocated_cost,0)) FROM olp.commitment_usage u JOIN olp.commitment_periods cp ON cp.id=u.period_id AND NOT cp.legacy LEFT JOIN olp.commitment_allocations a ON a.attempt_id=u.attempt_id
 WHERE u.observed_at >= (h).bucket AND u.observed_at < (h).bucket+interval '1 hour'
 AND NOT EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE f.attempt_id=u.attempt_id)
 AND u.payload->>'provider_id' IS NOT DISTINCT FROM (h).provider_id::text
 AND u.payload->>'route_slug' IS NOT DISTINCT FROM (h).route_slug
 AND u.payload->>'upstream_model' IS NOT DISTINCT FROM (h).upstream_model
 AND u.payload->>'operation' IS NOT DISTINCT FROM (h).operation
 AND u.payload->>'surface' IS NOT DISTINCT FROM (h).surface
 AND u.payload->>'api_key_id' IS NOT DISTINCT FROM (h).api_key_id::text
 AND u.payload->>'budget_group_id' IS NOT DISTINCT FROM (h).budget_group_id::text
 AND u.payload->'attribution' IS NOT DISTINCT FROM (h).attribution
 AND u.payload->>'end_user_digest' IS NOT DISTINCT FROM (h).end_user_digest
 AND u.payload->>'model_family' IS NOT DISTINCT FROM (h).model_family
 AND u.payload->>'estimate_provenance' IS NOT DISTINCT FROM (h).estimate_provenance
 AND (u.payload->>'budget_exempt')::boolean=(h).budget_exempt
 AND (u.payload->>'commitment_amount')::numeric IS NOT DISTINCT FROM (h).commitment_amount
 AND u.payload->>'commitment_period' IS NOT DISTINCT FROM (h).commitment_period
$$;

-- Old hourly evidence can establish weights, but cannot prove individual
-- completeness or reconstruct price-line detail. Keep that limitation explicit.
CREATE FUNCTION olp.backfill_commitment_hours(p_limit integer DEFAULT 512,p_cutoff timestamptz DEFAULT NULL) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE rec record; s olp.commitment_schedules; pid uuid; n integer:=0; consumer uuid; cname text;
BEGIN
 FOR rec IN SELECT h.ctid row_ctid,h.*,md5('commitment-hour/'||to_jsonb(h)::text)::uuid identity
  FROM olp.attempt_usage_hourly h WHERE h.commitment_backfill_pending AND h.commitment_period IS NOT NULL
   AND h.bucket>=olp.commitment_bucket(h.commitment_period,COALESCE(p_cutoff,now()-make_interval(days=>COALESCE(
    (SELECT value::integer FROM olp.settings WHERE key='retention.usage_days'),90))))
   AND NOT EXISTS(SELECT 1 FROM olp.commitment_usage u WHERE u.attempt_id=md5('commitment-hour/'||to_jsonb(h)::text)::uuid)
   AND EXISTS(SELECT 1 FROM olp.commitment_schedules cs WHERE cs.historical AND cs.provider_id=h.provider_id AND cs.period=h.commitment_period
    AND cs.effective_at<olp.commitment_end(h.commitment_period,olp.commitment_bucket(h.commitment_period,h.bucket))
    AND (cs.ends_at IS NULL OR cs.ends_at>h.bucket))
  ORDER BY h.bucket LIMIT p_limit FOR UPDATE OF h SKIP LOCKED
 LOOP
  SELECT * INTO s FROM olp.commitment_schedules cs WHERE cs.provider_id=rec.provider_id AND cs.period=rec.commitment_period
   AND cs.effective_at<olp.commitment_end(rec.commitment_period,olp.commitment_bucket(rec.commitment_period,rec.bucket))
   AND (cs.ends_at IS NULL OR cs.ends_at>rec.bucket) ORDER BY cs.effective_at DESC LIMIT 1;
  pid:=olp.ensure_commitment_period(s.id,olp.commitment_bucket(rec.commitment_period,rec.bucket));
  SELECT COALESCE(k.project_id,s.project_id,r.project_id) INTO consumer FROM (SELECT 1) x
   LEFT JOIN olp.api_keys k ON k.id=rec.api_key_id LEFT JOIN olp.routes r ON r.slug=rec.route_slug;
  SELECT name INTO cname FROM olp.projects WHERE id=consumer;
  INSERT INTO olp.commitment_usage(attempt_id,period_id,observed_at,weight,eligible,complete,payload)
  VALUES(rec.identity,pid,rec.bucket,rec.commitment_weight,NOT rec.budget_exempt,false,
   (to_jsonb(rec)-'identity'-'row_ctid')||jsonb_build_object('attempt_id',rec.identity,'observed_at',rec.bucket,'charge_status','billable',
    'usage_complete',true,'caller_credential',false,'commitment_schedule_id',s.id,
    'consumer',consumer,'consumer_name',cname,'connection_name',s.provider_name,
    'provider_kind',s.provider_kind,'vendor_id',s.vendor_id,'provider_project',s.project_id)) ON CONFLICT DO NOTHING;
  UPDATE olp.commitment_periods SET complete=false WHERE id=pid;
  PERFORM olp.dirty_commitment(olp.commitment_bucket(rec.commitment_period,rec.bucket),olp.commitment_end(rec.commitment_period,olp.commitment_bucket(rec.commitment_period,rec.bucket)));
  UPDATE olp.attempt_usage_hourly SET commitment_backfill_pending=false WHERE ctid=rec.row_ctid;
  n:=n+1;
 END LOOP;
 RETURN n;
END $$;
