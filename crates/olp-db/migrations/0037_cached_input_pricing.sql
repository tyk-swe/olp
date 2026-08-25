-- Cached input tokens are discounted to 10-25% of the input rate by every
-- provider that offers prompt caching, but the cost expression multiplied the
-- whole (cache-inclusive) input count by the full input rate. Give `prices` a
-- cached tier so the estimate can reflect what the provider actually charges.
--
-- The column is nullable and revisions written before this migration keep it
-- NULL, which continues to bill cached tokens at the full input rate. Existing
-- `estimated_cost` history is left untouched: it is a record of what was
-- computed at the time, not a derived view.
ALTER TABLE prices ADD COLUMN cached_input_per_million numeric(24, 12);
