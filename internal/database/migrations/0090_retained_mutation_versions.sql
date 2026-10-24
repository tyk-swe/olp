-- Reads may refresh observation timestamps while a paid mutation is in flight.
-- Only another paid mutation advances this fence.
ALTER TABLE olp.provider_resources
 ADD COLUMN mutation_version bigint NOT NULL DEFAULT 0 CHECK(mutation_version>=0);
