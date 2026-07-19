-- Preserve existing global/client rows until their normal expiry while allowing
-- new binaries to write only source-aware buckets. The action and scope checks
-- are expanded rather than rebuilt so this migration is safe during a rolling
-- upgrade with N-1 control-plane replicas.
ALTER TABLE public_auth_rate_limits
    DROP CONSTRAINT IF EXISTS public_auth_rate_limits_action_check,
    DROP CONSTRAINT IF EXISTS public_auth_rate_limits_scope_check;

ALTER TABLE public_auth_rate_limits
    ADD CONSTRAINT public_auth_rate_limits_action_check
        CHECK (action IN ('local_login', 'invitation_acceptance', 'oidc_login')),
    ADD CONSTRAINT public_auth_rate_limits_scope_check
        CHECK (scope IN ('global', 'client', 'source', 'source_target'));
