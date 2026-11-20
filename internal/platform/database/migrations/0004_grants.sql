-- Grants beneath immutable credential versions (ADR 0008).
--
-- Grant enrollment creates an ordinary credential version that records the
-- plugin, the observed principal and the grant facts. Its secret, under the
-- provider_credential purpose, is what gateways serve: the grant's current
-- access token with its facts. The grant's refresh token is kept apart under
-- its own purpose, which gateway code never reads.
ALTER TABLE olp.secrets
    DROP CONSTRAINT secrets_purpose_check,
    ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN ('oidc_client', 'oidc_flow', 'mutation_replay',
        'provider_credential', 'notification_secret', 'provider_continuation', 'media_job_source',
        'provider_grant_refresh', 'grant_enrollment')),
    ADD CONSTRAINT secrets_grant_enrollment_bound
        CHECK (purpose <> 'grant_enrollment'
               OR (expires_at IS NOT NULL AND octet_length(ciphertext) <= 65536));

ALTER TABLE olp.provider_credentials
    ADD COLUMN plugin_digest text,
    ADD COLUMN principal text,
    ADD COLUMN grant_facts jsonb,
    ADD CONSTRAINT provider_credentials_grant_check CHECK (
        (plugin_digest IS NULL) = (principal IS NULL) AND (plugin_digest IS NULL) = (grant_facts IS NULL));

-- A credential version never changes once created, except to be revoked:
-- published revisions and attempts rely on what it was.
CREATE FUNCTION olp.preserve_provider_credential() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL
       OR to_jsonb(NEW) - 'revoked_at' IS DISTINCT FROM to_jsonb(OLD) - 'revoked_at' THEN
        RAISE EXCEPTION 'a credential version changes only by revocation'
            USING ERRCODE = '23514', CONSTRAINT = 'provider_credential_immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER preserve_provider_credential BEFORE UPDATE ON olp.provider_credentials
FOR EACH ROW EXECUTE FUNCTION olp.preserve_provider_credential();

-- The grant beneath a credential version: its access token's expiry, and the
-- secret holding its refresh token when the upstream issued one.
CREATE TABLE olp.provider_grants (
    credential_id uuid PRIMARY KEY REFERENCES olp.provider_credentials ON DELETE CASCADE,
    refresh_token_id uuid UNIQUE REFERENCES olp.secrets,
    expires_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A grant enrollment in progress. The plugin's session state is encrypted
-- under the same id with the grant_enrollment purpose and the same expiry.
-- Continuing claims it once; the row outlives the state for an hour so a
-- late or repeated continuation learns why it failed.
CREATE TABLE olp.grant_enrollments (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES olp.providers ON DELETE CASCADE,
    slot_id uuid NOT NULL REFERENCES olp.provider_slots ON DELETE CASCADE,
    plugin_digest text NOT NULL,
    profile_id text NOT NULL,
    -- A user or management token, like a replay actor.
    started_by uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    continued_at timestamptz
);
CREATE INDEX grant_enrollments_expiry ON olp.grant_enrollments(expires_at);
