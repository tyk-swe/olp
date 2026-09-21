ALTER TABLE olp_go.prices
 ADD COLUMN cache_write_input_per_million numeric(24,12),
 ADD COLUMN cache_write_5m_input_per_million numeric(24,12),
 ADD COLUMN cache_write_1h_input_per_million numeric(24,12);
ALTER TABLE olp_go.attempt_usage_facts
 ADD COLUMN cache_write_input_tokens bigint CHECK (cache_write_input_tokens IS NULL OR cache_write_input_tokens>=0),
 ADD COLUMN cache_write_5m_input_tokens bigint CHECK (cache_write_5m_input_tokens IS NULL OR cache_write_5m_input_tokens>=0),
 ADD COLUMN cache_write_1h_input_tokens bigint CHECK (cache_write_1h_input_tokens IS NULL OR cache_write_1h_input_tokens>=0),
 ADD CONSTRAINT attempt_usage_cache_subset CHECK (
   (cached_input_tokens IS NULL AND cache_write_input_tokens IS NULL) OR
   (input_tokens IS NOT NULL AND
    COALESCE(cached_input_tokens,0)+COALESCE(cache_write_input_tokens,0)<=input_tokens)),
 ADD CONSTRAINT attempt_usage_cache_detail_subset CHECK (
   (cache_write_5m_input_tokens IS NULL AND cache_write_1h_input_tokens IS NULL) OR
   (cache_write_input_tokens IS NOT NULL AND
    COALESCE(cache_write_5m_input_tokens,0)+COALESCE(cache_write_1h_input_tokens,0)<=cache_write_input_tokens));
ALTER TABLE olp_go.attempt_usage_hourly
 ADD COLUMN cache_write_input_tokens numeric(30,0) NOT NULL DEFAULT 0 CHECK (cache_write_input_tokens>=0),
 ADD COLUMN cache_write_5m_input_tokens numeric(30,0) NOT NULL DEFAULT 0 CHECK (cache_write_5m_input_tokens>=0),
 ADD COLUMN cache_write_1h_input_tokens numeric(30,0) NOT NULL DEFAULT 0 CHECK (cache_write_1h_input_tokens>=0);
