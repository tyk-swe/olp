-- Budget admission distinguishes a consumer that is merely busy from one
-- whose undelivered backlog is stranded: the lagged entries' own stream ids
-- carry their age, and the health row keeps the oldest one.
ALTER TABLE olp.request_metadata_consumer_health
    ADD COLUMN oldest_lagged_at timestamptz;
