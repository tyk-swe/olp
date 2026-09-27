-- Grant enrollment by device authorization.
--
-- The operator approves a device authorization upstream while status requests
-- poll the plugin, one poll per interval, so any control replica serves the
-- next one. poll_at is when the next status request may poll: a lease while a
-- poll runs, then the interval after it. An enrollment of either kind records
-- how it ended once it did: completed with the credential version its grant
-- created, or denied upstream.
ALTER TABLE olp.grant_enrollments
    ADD COLUMN poll_interval integer CHECK (poll_interval > 0),
    ADD COLUMN poll_at timestamptz,
    ADD COLUMN outcome text CHECK (outcome IN ('completed', 'denied')),
    ADD COLUMN credential_id uuid REFERENCES olp.provider_credentials ON DELETE CASCADE,
    ADD CONSTRAINT grant_enrollments_polling_check CHECK ((poll_interval IS NULL) = (poll_at IS NULL)),
    ADD CONSTRAINT grant_enrollments_completion_check CHECK (
        (outcome IS NOT DISTINCT FROM 'completed') = (credential_id IS NOT NULL)
        AND (outcome IS NULL OR continued_at IS NOT NULL));
