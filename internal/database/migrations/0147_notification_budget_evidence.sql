ALTER TABLE olp.notification_deliveries
 ADD COLUMN budget_subject_kind text,
 ADD COLUMN budget_subject_id uuid,
 ADD COLUMN budget_window_kind text;

UPDATE olp.notification_deliveries v
 SET budget_subject_kind=r.subject_kind,budget_subject_id=r.subject_id,budget_window_kind=r.window_kind
 FROM olp.notification_rules r WHERE r.id=v.rule_id AND v.window_id IS NOT NULL;

CREATE FUNCTION olp.snapshot_budget_notification_subject() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.window_id IS NOT NULL AND num_nonnulls(NEW.budget_subject_kind,NEW.budget_subject_id,NEW.budget_window_kind)=0 THEN
  SELECT subject_kind,subject_id,window_kind INTO NEW.budget_subject_kind,NEW.budget_subject_id,NEW.budget_window_kind
  FROM olp.notification_rules WHERE id=NEW.rule_id FOR SHARE;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER notification_budget_subject_snapshot BEFORE INSERT ON olp.notification_deliveries
 FOR EACH ROW EXECUTE FUNCTION olp.snapshot_budget_notification_subject();

ALTER TABLE olp.notification_deliveries ADD CONSTRAINT notification_budget_subject_check CHECK (
 (window_id IS NULL AND num_nonnulls(budget_subject_kind,budget_subject_id,budget_window_kind)=0)
 OR (window_id IS NOT NULL AND num_nonnulls(budget_subject_kind,budget_subject_id,budget_window_kind)=3
 AND budget_subject_kind IN ('api_key','budget_group') AND budget_window_kind IN ('day','week','month')));
