CREATE TABLE olp.code_accounts (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects,
    provider_id uuid NOT NULL REFERENCES olp.providers,
    credential_id uuid NOT NULL REFERENCES olp.provider_credentials,
    principal text NOT NULL,
    name text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    models jsonb NOT NULL,
    allowance jsonb,
    health text NOT NULL DEFAULT 'unknown' CHECK (health IN ('unknown','healthy','unavailable','quota_limited')),
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, provider_id, principal)
);

CREATE FUNCTION olp.preserve_code_account() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.project_id <> OLD.project_id OR NEW.provider_id <> OLD.provider_id OR NEW.principal <> OLD.principal THEN
        RAISE EXCEPTION 'code account identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER preserve_code_account BEFORE UPDATE ON olp.code_accounts
    FOR EACH ROW EXECUTE FUNCTION olp.preserve_code_account();

CREATE TABLE olp.code_pools (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects,
    name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('personal','shared')),
    owner_user_id uuid REFERENCES olp.users,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    CHECK ((kind='personal') = (owner_user_id IS NOT NULL))
);
CREATE TABLE olp.code_pool_accounts (
    pool_id uuid NOT NULL REFERENCES olp.code_pools,
    account_id uuid NOT NULL REFERENCES olp.code_accounts,
    PRIMARY KEY(pool_id,account_id)
);
CREATE TABLE olp.code_pool_keys (
    pool_id uuid NOT NULL REFERENCES olp.code_pools,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    PRIMARY KEY(pool_id,api_key_id)
);
CREATE TABLE olp.code_routes (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects,
    slug text NOT NULL UNIQUE,
    draft jsonb NOT NULL,
    etag uuid NOT NULL,
    latest_revision_id uuid,
    created_by uuid NOT NULL REFERENCES olp.users
);
CREATE TABLE olp.code_route_revisions (
    id uuid PRIMARY KEY,
    route_id uuid NOT NULL REFERENCES olp.code_routes,
    revision integer NOT NULL,
    document jsonb NOT NULL,
    published_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid NOT NULL REFERENCES olp.users,
    UNIQUE(route_id,revision)
);
ALTER TABLE olp.code_routes ADD FOREIGN KEY(latest_revision_id) REFERENCES olp.code_route_revisions;

CREATE TABLE olp.code_bindings (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects,
    route_id uuid NOT NULL REFERENCES olp.code_routes,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    conversation text NOT NULL CHECK (octet_length(conversation) BETWEEN 1 AND 256),
    parent_id uuid REFERENCES olp.code_bindings,
    root_id uuid NOT NULL REFERENCES olp.code_bindings,
    account_id uuid NOT NULL REFERENCES olp.code_accounts,
    principal text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    retired_at timestamptz,
    UNIQUE(route_id,api_key_id,conversation)
);
CREATE INDEX code_bindings_root ON olp.code_bindings(root_id);
CREATE FUNCTION olp.preserve_code_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'code binding retirement is durable' USING ERRCODE = '23514';
    END IF;
    IF OLD.retired_at IS NOT NULL OR NEW.retired_at IS NULL OR
       to_jsonb(NEW)-'retired_at' IS DISTINCT FROM to_jsonb(OLD)-'retired_at' THEN
        RAISE EXCEPTION 'code binding changes only by retirement' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER preserve_code_binding BEFORE UPDATE OR DELETE ON olp.code_bindings
    FOR EACH ROW EXECUTE FUNCTION olp.preserve_code_binding();

CREATE TABLE olp.code_token_budgets (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects,
    route_id uuid REFERENCES olp.code_routes,
    api_key_id uuid REFERENCES olp.api_keys,
    daily_tokens bigint CHECK (daily_tokens BETWEEN 1 AND 9007199254740991),
    monthly_tokens bigint CHECK (monthly_tokens BETWEEN 1 AND 9007199254740991),
    enabled boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    CHECK (daily_tokens IS NOT NULL OR monthly_tokens IS NOT NULL)
);
CREATE TABLE olp.code_token_windows (
    budget_id uuid NOT NULL REFERENCES olp.code_token_budgets,
    period text NOT NULL CHECK (period IN ('day','month')),
    starts_at timestamptz NOT NULL,
    reserved bigint NOT NULL DEFAULT 0 CHECK (reserved>=0),
    measured bigint NOT NULL DEFAULT 0 CHECK (measured>=0),
    PRIMARY KEY(budget_id,period,starts_at)
);
CREATE TABLE olp.code_attempts (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects,
    route_id uuid NOT NULL REFERENCES olp.code_routes,
    route_revision_id uuid NOT NULL REFERENCES olp.code_route_revisions,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    binding_id uuid NOT NULL REFERENCES olp.code_bindings,
    account_id uuid NOT NULL REFERENCES olp.code_accounts,
    operation text NOT NULL CHECK (octet_length(operation) BETWEEN 1 AND 100),
    model text NOT NULL CHECK (octet_length(model) BETWEEN 1 AND 200),
    state text NOT NULL CHECK (state IN ('prepared','uncertain','settled','aborted','bound_violation')),
    reserved_tokens bigint NOT NULL CHECK (reserved_tokens>=0),
    reported_tokens bigint CHECK (reported_tokens>=0),
    input_tokens bigint, output_tokens bigint, cached_tokens bigint, reasoning_tokens bigint,
    bound_evidence text,
    refusal text,
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE TABLE olp.code_token_reservations (
    attempt_id uuid NOT NULL REFERENCES olp.code_attempts,
    budget_id uuid NOT NULL,
    period text NOT NULL,
    starts_at timestamptz NOT NULL,
    tokens bigint NOT NULL CHECK (tokens>0),
    PRIMARY KEY(attempt_id,budget_id,period),
    FOREIGN KEY(budget_id,period,starts_at) REFERENCES olp.code_token_windows
);
CREATE TABLE olp.code_refusals (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES olp.projects,
    route_id uuid NOT NULL REFERENCES olp.code_routes,
    api_key_id uuid NOT NULL REFERENCES olp.api_keys,
    code text NOT NULL CHECK (code ~ '^code_[a-z_]{1,80}$'),
    occurred_at timestamptz NOT NULL DEFAULT now()
);
