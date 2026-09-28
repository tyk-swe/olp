-- A grant enrollment by device authorization records that it expired, as it
-- records a denial: whether the upstream reported the expiry to a poll, or the
-- first status request after the enrollment's own expiry found it, the
-- enrollment ends once, and its failure is audited once.
ALTER TABLE olp.grant_enrollments
    DROP CONSTRAINT grant_enrollments_outcome_check,
    ADD CONSTRAINT grant_enrollments_outcome_check CHECK (outcome IN ('completed', 'denied', 'expired'));
