ALTER TABLE olp.notification_destinations ADD COLUMN type text NOT NULL DEFAULT 'webhook'
 CHECK(type IN ('webhook','slack','msteams','discord','pagerduty','email')),
 ADD COLUMN configuration jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(configuration)='object' AND octet_length(configuration::text)<=4096);
ALTER TABLE olp.worker_task_health ADD COLUMN first_seen_at timestamptz NOT NULL DEFAULT now();
UPDATE olp.worker_task_health SET first_seen_at=LEAST(first_seen_at,checked_at);
CREATE INDEX requests_completed_route_idx ON olp.requests(completed_at,route_slug) WHERE completed_at IS NOT NULL;
CREATE INDEX attempts_completed_provider_idx ON olp.attempts(completed_at,provider_id) WHERE completed_at IS NOT NULL;
ALTER TABLE olp.notification_rules DROP CONSTRAINT notification_rules_event_check,
 ADD CONSTRAINT notification_rules_event_check CHECK(event IN ('budget.threshold','provider.grant.lapsed','key.expiring','budget.exhausted',
 'provider.circuit.open','provider.circuit.closed','provider.error_rate','route.latency','provider.credential.failing',
 'model.retirement','runtime.install_failed','worker.stale','report.spend')),
 ADD COLUMN configuration jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(configuration)='object' AND octet_length(configuration::text)<=4096);
ALTER TABLE olp.notification_rules DROP CONSTRAINT notification_rules_subject_check,
 ADD CONSTRAINT notification_rules_subject_check CHECK(CASE event
 WHEN 'budget.threshold' THEN num_nonnulls(subject_kind,subject_id,window_kind,threshold_percent)=4
 WHEN 'key.expiring' THEN subject_kind IS NOT DISTINCT FROM 'api_key' AND subject_id IS NOT NULL AND num_nonnulls(window_kind,threshold_percent)=0
 ELSE num_nonnulls(subject_kind,subject_id,window_kind,threshold_percent)=0
 AND (event IN ('budget.exhausted','route.latency','report.spend','model.retirement') OR project_id IS NULL) END);
ALTER TABLE olp.notification_deliveries ADD COLUMN event text,
 ADD COLUMN dedup_key text CHECK(length(dedup_key)<=512),
 ADD COLUMN resolved boolean NOT NULL DEFAULT false;
ALTER TABLE olp.notification_deliveries DROP CONSTRAINT notification_deliveries_evidence_check,
 ADD CONSTRAINT notification_deliveries_evidence_check CHECK (
 num_nonnulls(window_id,threshold_percent,accrued,limit_amount)=4 AND num_nonnulls(credential_id,payload,api_key_id,due_at,reason,event,dedup_key)=0
 OR num_nonnulls(window_id,threshold_percent,accrued,limit_amount,currency,api_key_id,due_at,reason,event,dedup_key)=0 AND num_nonnulls(credential_id,payload)=2
 OR num_nonnulls(window_id,threshold_percent,accrued,limit_amount,currency,credential_id,event,dedup_key)=0 AND num_nonnulls(api_key_id,due_at,reason,payload)=4
 OR num_nonnulls(window_id,threshold_percent,accrued,limit_amount,currency,credential_id,api_key_id,due_at,reason)=0 AND num_nonnulls(event,dedup_key,payload)=3);
CREATE UNIQUE INDEX notification_deliveries_signal ON olp.notification_deliveries(rule_id,dedup_key,resolved) WHERE dedup_key IS NOT NULL;
CREATE TABLE olp.notification_signal_states (
 rule_id uuid NOT NULL REFERENCES olp.notification_rules ON DELETE CASCADE,
 subject text NOT NULL CHECK(length(subject)<=512),
 active boolean NOT NULL DEFAULT false,
 incident bigint NOT NULL DEFAULT 0 CHECK(incident>=0),
 last_trigger_at timestamptz,
 checked_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(rule_id,subject)
);
CREATE TABLE olp.runtime_install_status (
 gateway_instance text PRIMARY KEY CHECK(length(gateway_instance) BETWEEN 1 AND 256),
 desired_generation bigint NOT NULL,
 installed_generation bigint NOT NULL,
 failed boolean NOT NULL,
 checked_at timestamptz NOT NULL DEFAULT now()
);
