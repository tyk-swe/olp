ALTER TABLE olp.provider_resources
    DROP CONSTRAINT provider_resources_kind_check,
    ADD CONSTRAINT provider_resources_kind_check CHECK (kind IN (
        'file', 'batch', 'response', 'continuation', 'strict_response',
        'interaction', 'strict_file', 'strict_batch', 'anthropic_file',
        'anthropic_batch', 'gemini_upload', 'gemini_file', 'gemini_batch',
        'cloud_batch'
    ));

CREATE TABLE olp.batch_settlements (
    resource_id uuid PRIMARY KEY,
    kind text NOT NULL,
    adapter text NOT NULL CHECK (adapter IN (
        'openai', 'together', 'mistral', 'gemini_managed', 'anthropic',
        'gemini', 'vertex', 'bedrock'
    )),
    api_key_id uuid NOT NULL,
    route_slug text NOT NULL,
    route_revision_id uuid NOT NULL,
    provider_id uuid NOT NULL,
    provider_revision_id uuid NOT NULL,
    target_id text NOT NULL,
    slot_id uuid NOT NULL,
    credential_id uuid,
    upstream_id text CHECK (
        upstream_id IS NULL OR octet_length(upstream_id) BETWEEN 1 AND 512
    ),
    surface text NOT NULL CHECK (surface IN ('openai', 'anthropic', 'gemini', 'native')),
    submission_digest bytea NOT NULL,
    line_operation text NOT NULL CHECK (line_operation IN ('generation', 'embeddings')),
    template jsonb NOT NULL,
    state text NOT NULL CHECK (state IN (
        'submitting', 'submission_unknown', 'active', 'settling',
        'settled', 'uncertain', 'conflict'
    )),
    expected_results bigint CHECK (expected_results >= 0),
    settled_results bigint NOT NULL DEFAULT 0,
    uncertain_results bigint NOT NULL DEFAULT 0,
    cursor jsonb NOT NULL DEFAULT '{}',
    claim_id uuid,
    claimed_until timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error_class text CHECK (
        last_error_class IS NULL OR octet_length(last_error_class) <= 120
    ),
    close_after timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    closed_at timestamptz,
    CHECK ((claim_id IS NULL) = (claimed_until IS NULL)),
    CHECK ((state IN ('settled', 'uncertain')) = (closed_at IS NOT NULL))
);
CREATE UNIQUE INDEX batch_settlements_submission_identity
    ON olp.batch_settlements(api_key_id, submission_digest)
    WHERE state IN ('submitting', 'submission_unknown');
CREATE INDEX batch_settlements_due
    ON olp.batch_settlements(next_attempt_at, resource_id)
    WHERE state IN ('submitting', 'submission_unknown', 'active', 'settling');
CREATE INDEX batch_settlements_closed
    ON olp.batch_settlements(closed_at)
    WHERE closed_at IS NOT NULL;
CREATE INDEX batch_settlements_owner
    ON olp.batch_settlements(api_key_id, created_at DESC);

CREATE TABLE olp.batch_settlement_results (
    resource_id uuid NOT NULL REFERENCES olp.batch_settlements(resource_id) ON DELETE CASCADE,
    custom_id_digest bytea NOT NULL,
    result_kind text NOT NULL CHECK (result_kind IN (
        'succeeded', 'errored', 'cancelled', 'expired', 'aggregate', 'gap'
    )),
    event_id uuid NOT NULL UNIQUE,
    evidence_digest bytea NOT NULL,
    settled_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (resource_id, custom_id_digest, result_kind)
);

CREATE TABLE olp.batch_pending_subjects (
    resource_id uuid NOT NULL REFERENCES olp.batch_settlements(resource_id) ON DELETE CASCADE,
    ordinal integer NOT NULL CHECK (ordinal > 0),
    api_key_id uuid NOT NULL,
    budget_group_id uuid,
    project_id uuid,
    api_key_project_id uuid,
    route_id uuid,
    leg_route_id uuid,
    pinned_route_id uuid,
    budget_owner_ids uuid[] NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL,
    slot_id uuid,
    attribution jsonb NOT NULL,
    end_user_digest text,
    PRIMARY KEY (resource_id, ordinal)
);
CREATE INDEX batch_pending_usage_key ON olp.batch_pending_subjects(api_key_id, route_slug);
CREATE INDEX batch_pending_usage_key_digest ON olp.batch_pending_subjects(api_key_id, end_user_digest);
CREATE INDEX batch_pending_usage_attribution ON olp.batch_pending_subjects USING gin(attribution jsonb_path_ops);
CREATE INDEX batch_pending_usage_group ON olp.batch_pending_subjects(budget_group_id);
CREATE INDEX batch_pending_usage_project ON olp.batch_pending_subjects(project_id);
CREATE INDEX batch_pending_usage_key_project ON olp.batch_pending_subjects(api_key_project_id, end_user_digest);
CREATE INDEX batch_pending_usage_provider ON olp.batch_pending_subjects(provider_id);
CREATE INDEX batch_pending_usage_slot ON olp.batch_pending_subjects(slot_id);
CREATE INDEX batch_pending_usage_route ON olp.batch_pending_subjects(route_id);
CREATE INDEX batch_pending_usage_pinned_route ON olp.batch_pending_subjects(pinned_route_id);
CREATE INDEX batch_pending_usage_leg_route ON olp.batch_pending_subjects(leg_route_id);

CREATE TABLE olp.batch_pending_recorded_owners (
    resource_id uuid NOT NULL,
    ordinal integer NOT NULL,
    budget_owner_id uuid NOT NULL,
    PRIMARY KEY (resource_id, ordinal, budget_owner_id),
    FOREIGN KEY (resource_id, ordinal)
        REFERENCES olp.batch_pending_subjects(resource_id, ordinal) ON DELETE CASCADE
);
CREATE INDEX batch_pending_usage_recorded_owners
    ON olp.batch_pending_recorded_owners(budget_owner_id);

CREATE FUNCTION olp.project_batch_pending_recorded_owners() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO olp.batch_pending_recorded_owners(resource_id, ordinal, budget_owner_id)
    SELECT NEW.resource_id, NEW.ordinal, owner FROM unnest(NEW.budget_owner_ids) owner
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END
$$;
CREATE TRIGGER batch_pending_recorded_owners
    AFTER INSERT ON olp.batch_pending_subjects
    FOR EACH ROW EXECUTE FUNCTION olp.project_batch_pending_recorded_owners();

CREATE FUNCTION olp.batch_settlement_subjects(bs olp.batch_settlements)
RETURNS SETOF olp.batch_pending_subjects LANGUAGE sql STABLE AS $$
    SELECT bs.resource_id,
           (a->>'ordinal')::integer,
           bs.api_key_id,
           NULLIF(bs.template->>'budget_group_id', '')::uuid,
           COALESCE(k.project_id, p.project_id, r.project_id),
           k.project_id,
           r.id,
           leg_r.id,
           pinned_r.id,
           ARRAY(
               SELECT b::uuid
               FROM jsonb_array_elements_text(COALESCE(a->'routing'->'budgets', '[]'::jsonb)) b
           ),
           bs.template->>'route_slug',
           (a->>'provider_id')::uuid,
           NULLIF(a->'routing'->>'credential_slot_id', '')::uuid,
           COALESCE(bs.template->'attribution', '{}'::jsonb),
           NULLIF(bs.template->>'end_user_digest', '')
    FROM jsonb_array_elements(
        CASE WHEN jsonb_typeof(bs.template->'attempts') = 'array'
             THEN bs.template->'attempts' ELSE '[]'::jsonb END
    ) a
    JOIN olp.api_keys k ON k.id = bs.api_key_id
    JOIN olp.providers p ON p.id = (a->>'provider_id')::uuid
    LEFT JOIN olp.routes r ON r.slug = bs.template->>'route_slug'
    LEFT JOIN olp.routes pinned_r ON pinned_r.slug = bs.route_slug
    LEFT JOIN olp.routes leg_r ON leg_r.slug = a->'routing'->'leg'->>'route'
    WHERE NOT COALESCE((a->'routing'->>'budget_exempt')::boolean, false);
$$;

CREATE FUNCTION olp.project_batch_settlement_subjects() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    was_open boolean;
    is_open boolean;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        was_open := OLD.state IN ('submitting', 'submission_unknown', 'active', 'settling', 'conflict');
        is_open := NEW.state IN ('submitting', 'submission_unknown', 'active', 'settling', 'conflict');
        IF was_open IS NOT DISTINCT FROM is_open
           AND NEW.template IS NOT DISTINCT FROM OLD.template THEN
            RETURN NEW;
        END IF;
    END IF;
    DELETE FROM olp.batch_pending_subjects WHERE resource_id = NEW.resource_id;
    IF NEW.state IN ('submitting', 'submission_unknown', 'active', 'settling', 'conflict') THEN
        INSERT INTO olp.batch_pending_subjects
        SELECT * FROM olp.batch_settlement_subjects(NEW);
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER project_batch_settlement_subjects
    AFTER INSERT OR UPDATE OF state, template ON olp.batch_settlements
    FOR EACH ROW EXECUTE FUNCTION olp.project_batch_settlement_subjects();

CREATE OR REPLACE FUNCTION olp.has_pending_resource_usage(owner uuid)
RETURNS boolean LANGUAGE plpgsql STABLE AS $$
DECLARE
    pending boolean;
    a olp.aggregate_budget_accounts;
    e olp.end_user_accounts;
    projects uuid[];
    column_name text;
BEGIN
    FOREACH column_name IN ARRAY ARRAY[
        'api_key_id', 'budget_group_id', 'provider_id', 'slot_id',
        'route_id', 'pinned_route_id', 'leg_route_id'
    ] LOOP
        EXECUTE format(
            'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE %1$I=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE %1$I=$1)',
            column_name
        ) INTO pending USING owner;
        IF pending THEN RETURN true; END IF;
    END LOOP;
    EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.pending_resource_recorded_owners WHERE budget_owner_id=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_recorded_owners WHERE budget_owner_id=$1)'
        INTO pending USING owner;
    IF pending THEN RETURN true; END IF;
    SELECT * INTO a FROM olp.aggregate_budget_accounts WHERE id = owner;
    IF FOUND THEN
        IF a.installation_id IS NOT NULL THEN
            EXECUTE 'SELECT
                (SELECT resource_id FROM olp.provider_resource_pending_usage ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL
                OR
                (SELECT resource_id FROM olp.batch_pending_subjects ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL'
                INTO pending;
        ELSIF a.organization_id IS NOT NULL THEN
            SELECT array_agg(id) INTO projects FROM olp.projects WHERE organization_id = a.organization_id;
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id = ANY($1))
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = ANY($1))'
                INTO pending USING projects;
        ELSIF a.project_id IS NOT NULL AND a.attribution_key IS NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id = $1)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1)'
                INTO pending USING a.project_id;
        ELSIF a.project_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id = $1 AND attribution @> $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1 AND attribution @> $2)'
                INTO pending USING a.project_id, jsonb_build_object(a.attribution_key, a.attribution_value);
        ELSIF a.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_id = $1 AND route_slug = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND route_slug = $2)'
                INTO pending USING a.api_key_id, a.route_slug;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    SELECT * INTO e FROM olp.end_user_accounts WHERE id = owner;
    IF FOUND THEN
        IF e.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.api_key_id, e.end_user_digest;
        ELSE
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_project_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_project_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.project_id, e.end_user_digest;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    RETURN false;
END
$$;

ALTER TABLE olp.worker_task_health
    DROP CONSTRAINT worker_task_health_task_check,
    ADD CONSTRAINT worker_task_health_task_check CHECK (task IN (
        'request_metadata_consumer', 'maintenance', 'cost_reconciliation',
        'request_metadata_gateway_epoch_detection', 'media_reconciliation',
        'notification_delivery', 'grant_refresh', 'health_probes',
        'export_delivery', 'managed_export_delivery', 'focus_export',
        'billing_import', 'batch_settlement'
    ));

ALTER TABLE olp.secrets
    DROP CONSTRAINT secrets_purpose_check,
    ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN (
        'provider_credential', 'provider_continuation', 'mfa_totp', 'mfa_webauthn',
        'notification_secret', 'mutation_replay', 'oidc_client', 'oidc_flow',
        'media_job_source', 'provider_grant_refresh', 'grant_enrollment',
        'saml_key', 'saml_flow', 'sink_credential', 'mcp_credential',
        'billing_credential', 'guardrail_credential', 'provider_upload_session'
    ));
