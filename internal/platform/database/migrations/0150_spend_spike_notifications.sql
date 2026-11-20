ALTER TABLE olp.notification_rules DROP CONSTRAINT notification_rules_event_check,
 ADD CONSTRAINT notification_rules_event_check CHECK(event IN ('budget.threshold','provider.grant.lapsed','key.expiring','budget.exhausted',
 'provider.circuit.open','provider.circuit.closed','provider.error_rate','route.latency','provider.credential.failing',
 'model.retirement','runtime.install_failed','worker.stale','report.spend','spend.spike'));
ALTER TABLE olp.notification_rules DROP CONSTRAINT notification_rules_subject_check,
 ADD CONSTRAINT notification_rules_subject_check CHECK(CASE event
 WHEN 'budget.threshold' THEN num_nonnulls(subject_kind,subject_id,window_kind,threshold_percent)=4
 WHEN 'key.expiring' THEN subject_kind IS NOT DISTINCT FROM 'api_key' AND subject_id IS NOT NULL AND num_nonnulls(window_kind,threshold_percent)=0
 WHEN 'spend.spike' THEN project_id IS NOT NULL AND num_nonnulls(subject_kind,subject_id,window_kind,threshold_percent)=0
 ELSE num_nonnulls(subject_kind,subject_id,window_kind,threshold_percent)=0
 AND (event IN ('budget.exhausted','route.latency','report.spend','model.retirement') OR project_id IS NULL) END);
