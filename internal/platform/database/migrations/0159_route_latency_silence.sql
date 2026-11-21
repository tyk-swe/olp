ALTER TABLE olp.notification_rules
 ADD COLUMN silence_starts_at timestamptz,
 ADD COLUMN silence_ends_at timestamptz,
 ADD CONSTRAINT notification_rules_silence_check CHECK (
  num_nonnulls(silence_starts_at,silence_ends_at)=0 OR
  (num_nonnulls(silence_starts_at,silence_ends_at)=2 AND event='route.latency' AND project_id IS NOT NULL
   AND isfinite(silence_starts_at) AND isfinite(silence_ends_at) AND silence_starts_at<silence_ends_at)
 );

ALTER TABLE olp.notification_signal_states
 ADD COLUMN trigger_pending boolean NOT NULL DEFAULT false;
