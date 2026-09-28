-- Grant enrollment may replace only the credential binding it started with.
-- Keep the expected value even if that credential is subsequently removed.
ALTER TABLE olp.grant_enrollments
    ADD COLUMN expected_credential_id uuid;
