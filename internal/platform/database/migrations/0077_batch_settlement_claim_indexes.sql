-- Unmapped conflicted or uncertain intents still have no known upstream
-- identity and must protect identical retries until their retention cutoff.
DROP INDEX olp.batch_settlements_submission_identity;
CREATE UNIQUE INDEX batch_settlements_submission_identity
    ON olp.batch_settlements(api_key_id, submission_digest)
    WHERE state IN ('submitting', 'submission_unknown')
       OR (state IN ('conflict', 'uncertain') AND upstream_id IS NULL);

DROP INDEX olp.batch_settlements_due;
CREATE INDEX batch_settlements_due
    ON olp.batch_settlements(next_attempt_at, resource_id)
    WHERE state IN ('submitting', 'submission_unknown', 'active', 'settling', 'conflict');
