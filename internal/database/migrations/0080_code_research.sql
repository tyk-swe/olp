-- Opaque transport state only. Tool arguments, results and credentials are
-- never stored. Expired state is reclaimed on session creation.
CREATE TABLE olp.code_research_sessions (
    id uuid PRIMARY KEY,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    revision_id uuid NOT NULL REFERENCES olp.code_route_revisions,
    service text NOT NULL CHECK (service IN ('search','reader','zread')),
    account_id uuid NOT NULL REFERENCES olp.code_accounts,
    credential_id uuid NOT NULL REFERENCES olp.provider_credentials,
    generation bigint NOT NULL CHECK (generation > 0),
    -- Empty for stateless upstreams; the OLP session still fences the caller.
    upstream_id text NOT NULL CHECK (octet_length(upstream_id) BETWEEN 0 AND 256),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX code_research_sessions_expiry ON olp.code_research_sessions(expires_at);
CREATE INDEX code_research_sessions_key ON olp.code_research_sessions(api_key_id);
