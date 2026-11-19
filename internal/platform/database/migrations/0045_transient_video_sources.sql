-- Native video documents can echo prompts and arbitrary provider output.
-- Lifecycle recovery uses typed job fields and never reads this saved copy.
ALTER TABLE olp.media_jobs DROP CONSTRAINT media_jobs_strict_source;
UPDATE olp.media_jobs SET native_source_id = NULL WHERE native_source_id IS NOT NULL;
DELETE FROM olp.secrets WHERE purpose = 'media_job_source';
