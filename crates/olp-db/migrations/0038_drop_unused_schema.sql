-- Retire schema that no code path can reach any more.
--
-- `probed` was a third capability source that nothing ever wrote: discovery and
-- validation write `declared`, certification writes `certified`. Narrow both
-- capability CHECK constraints to the two values that are actually stored.
ALTER TABLE model_capabilities
    DROP CONSTRAINT IF EXISTS model_capabilities_source_check,
    ADD CONSTRAINT model_capabilities_source_check
        CHECK (source IN ('declared', 'certified'));

ALTER TABLE provider_revision_capabilities
    DROP CONSTRAINT IF EXISTS provider_revision_capabilities_source_check,
    ADD CONSTRAINT provider_revision_capabilities_source_check
        CHECK (source IN ('declared', 'certified'));

-- Keyset pagination moved to `ORDER BY id`, so the `(created_at, id)` cursor
-- indexes are never chosen.
DROP INDEX IF EXISTS providers_created_cursor_idx;
DROP INDEX IF EXISTS provider_models_provider_cursor_idx;
DROP INDEX IF EXISTS route_drafts_created_cursor_idx;
DROP INDEX IF EXISTS api_keys_created_cursor_idx;
DROP INDEX IF EXISTS invitations_created_at_id_idx;
DROP INDEX IF EXISTS users_created_at_id_idx;
DROP INDEX IF EXISTS sessions_created_at_id_idx;

-- Reads of `usage_facts` are by id or by `observed_at` for the hourly rollup;
-- route-filtered usage queries run against `attempt_usage_facts`.
DROP INDEX IF EXISTS usage_facts_route_idx;

-- `transactional_outbox_pending_idx` is a strict prefix of the
-- `(created_at, id)` index added in 0030, under the same partial predicate.
DROP INDEX IF EXISTS transactional_outbox_pending_idx;

-- The per-browser OIDC limiter is gone: login flows are no longer persisted and
-- `client_digest` has been bound to NULL for every flow that is. Drop the
-- column, the constraints that mention it, and the indexes that served it.
DROP INDEX IF EXISTS oidc_authorization_flows_login_rate_idx;
DROP INDEX IF EXISTS oidc_authorization_flows_client_rate_idx;

ALTER TABLE oidc_authorization_flows
    DROP CONSTRAINT IF EXISTS oidc_authorization_flows_client_digest_length,
    DROP CONSTRAINT IF EXISTS oidc_authorization_flows_security_context,
    DROP COLUMN IF EXISTS client_digest,
    ADD CONSTRAINT oidc_authorization_flows_security_context CHECK (
        (
            purpose = 'login'
            AND actor_user_id IS NULL
            AND actor_session_id IS NULL
            AND actor_security_version IS NULL
            AND recent_auth_purpose IS NULL
            AND recent_auth_resource_id IS NULL
        ) OR (
            purpose = 'link'
            AND actor_user_id IS NOT NULL
            AND actor_session_id IS NOT NULL
            AND actor_security_version > 0
            AND recent_auth_purpose IS NULL
            AND recent_auth_resource_id IS NULL
        ) OR (
            purpose = 'reauthenticate'
            AND actor_user_id IS NOT NULL
            AND actor_session_id IS NOT NULL
            AND actor_security_version > 0
            AND recent_auth_purpose IN (
                'password_enrollment', 'oidc_link', 'oidc_unlink'
            )
            AND (
                (recent_auth_purpose = 'oidc_unlink' AND recent_auth_resource_id IS NOT NULL)
                OR (recent_auth_purpose <> 'oidc_unlink' AND recent_auth_resource_id IS NULL)
            )
        )
    );

-- 0025 kept the `client` admission bucket so N-1 replicas could finish their
-- windows. Nothing has written it since; any surviving row is long past its
-- ten-minute expiry, so delete the leftovers and close the scope set.
DELETE FROM public_auth_rate_limits WHERE scope = 'client';

ALTER TABLE public_auth_rate_limits
    DROP CONSTRAINT IF EXISTS public_auth_rate_limits_scope_check,
    ADD CONSTRAINT public_auth_rate_limits_scope_check
        CHECK (scope IN ('global', 'source', 'source_target'));
