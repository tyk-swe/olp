ALTER TABLE olp.api_key_overlaps
 ADD COLUMN last_observed_use timestamptz,
 ADD COLUMN last_recording_error_at timestamptz;
