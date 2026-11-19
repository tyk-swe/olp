-- Admission identity stays stable as secret lookup segments rotate.
ALTER TABLE olp.api_keys ADD COLUMN limit_lookup_id text;
CREATE TABLE olp.api_key_overlaps (
 lookup_id text PRIMARY KEY,
 api_key_id uuid NOT NULL REFERENCES olp.api_keys ON DELETE CASCADE,
 digest bytea NOT NULL CHECK (octet_length(digest)=32),
 expires_at timestamptz NOT NULL
);
CREATE INDEX api_key_overlaps_owner ON olp.api_key_overlaps(api_key_id);
CREATE INDEX api_key_overlaps_expiry ON olp.api_key_overlaps(expires_at);

ALTER TABLE olp.notification_rules DROP CONSTRAINT notification_rules_event_check;
ALTER TABLE olp.notification_rules ADD CHECK (event IN ('budget.threshold','provider.grant.lapsed','key.expiring'));
ALTER TABLE olp.notification_rules DROP CONSTRAINT notification_rules_subject_check;
ALTER TABLE olp.notification_rules ADD CONSTRAINT notification_rules_subject_check CHECK (CASE event
 WHEN 'budget.threshold' THEN num_nonnulls(subject_kind,subject_id,window_kind,threshold_percent)=4
 WHEN 'key.expiring' THEN subject_kind IS NOT DISTINCT FROM 'api_key' AND subject_id IS NOT NULL AND num_nonnulls(window_kind,threshold_percent)=0
 ELSE project_id IS NULL AND num_nonnulls(subject_kind,subject_id,window_kind,threshold_percent)=0 END);
DROP INDEX olp.notification_rules_provider_event;
CREATE UNIQUE INDEX notification_rules_provider_event ON olp.notification_rules(event,destination_id) WHERE event='provider.grant.lapsed';
CREATE UNIQUE INDEX notification_rules_key_event ON olp.notification_rules(event,subject_id,destination_id) WHERE event='key.expiring';
ALTER TABLE olp.notification_deliveries ADD COLUMN api_key_id uuid REFERENCES olp.api_keys ON DELETE CASCADE,
 ADD COLUMN due_at timestamptz, ADD COLUMN reason text CHECK(reason IN ('expiry','rotation'));
ALTER TABLE olp.notification_deliveries DROP CONSTRAINT notification_deliveries_evidence_check;
ALTER TABLE olp.notification_deliveries ADD CONSTRAINT notification_deliveries_evidence_check CHECK (
 num_nonnulls(window_id,threshold_percent,accrued,limit_amount)=4 AND num_nonnulls(credential_id,payload,api_key_id,due_at,reason)=0
 OR num_nonnulls(window_id,threshold_percent,accrued,limit_amount,currency,api_key_id,due_at,reason)=0 AND num_nonnulls(credential_id,payload)=2
 OR num_nonnulls(window_id,threshold_percent,accrued,limit_amount,currency,credential_id)=0 AND num_nonnulls(api_key_id,due_at,reason,payload)=4);
CREATE UNIQUE INDEX notification_deliveries_key_due ON olp.notification_deliveries(rule_id,api_key_id,reason,due_at) WHERE api_key_id IS NOT NULL;
ALTER TABLE olp.notification_deliveries DROP CONSTRAINT budget_alert_deliveries_status_check;
ALTER TABLE olp.notification_deliveries ADD CHECK(status IN ('pending','delivered','failed','cancelled'));
