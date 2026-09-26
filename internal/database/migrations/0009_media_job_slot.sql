-- Retain the quota identity independently of credential rotation or removal.
-- Older jobs resolve their slot from the retained generation only when unique.
ALTER TABLE olp.media_jobs ADD COLUMN slot_id uuid;
