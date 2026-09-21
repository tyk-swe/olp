CREATE TABLE olp_go.management_tokens (
    id uuid PRIMARY KEY,
    lookup_id text NOT NULL UNIQUE,
    digest bytea NOT NULL,
    name text NOT NULL,
    scopes jsonb NOT NULL,
    created_by uuid NOT NULL REFERENCES olp_go.users,
    etag uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE olp_go.audit ADD COLUMN actor_management_token_id uuid REFERENCES olp_go.management_tokens;
ALTER TABLE olp_go.audit ADD CONSTRAINT audit_one_actor_check CHECK (
    NOT (actor_user_id IS NOT NULL AND actor_management_token_id IS NOT NULL));
ALTER TABLE olp_go.replays DROP CONSTRAINT replays_actor_fkey;
ALTER TABLE olp_go.users DROP CONSTRAINT users_role_management_check;
ALTER TABLE olp_go.users ADD CONSTRAINT users_role_management_check CHECK (role_management IN ('local','oidc','provisioned'));
CREATE TABLE olp_go.provisioned_users (
    source text NOT NULL,
    external_id text NOT NULL,
    user_id uuid NOT NULL UNIQUE REFERENCES olp_go.users,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(source, external_id),
    CHECK (source <> '' AND octet_length(source) <= 100 AND external_id <> '' AND octet_length(external_id) <= 255)
);
