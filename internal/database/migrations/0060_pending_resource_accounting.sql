-- pending_usage remains the sole accounting owner. This indexed projection
-- makes its unsettled subjects visible to budgets enabled after acceptance.
-- Native metadata on other resource kinds is opaque, including equal names.
CREATE FUNCTION olp.resource_owns_pending_usage(kind text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT kind IN ('response','strict_response','interaction');
$$;

CREATE TABLE olp.provider_resource_pending_usage (
    resource_id uuid NOT NULL REFERENCES olp.provider_resources(id) ON DELETE CASCADE,
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
CREATE INDEX pending_usage_key ON olp.provider_resource_pending_usage(api_key_id, route_slug);
CREATE INDEX pending_usage_key_digest ON olp.provider_resource_pending_usage(api_key_id,end_user_digest);
CREATE INDEX pending_usage_attribution ON olp.provider_resource_pending_usage USING gin(attribution jsonb_path_ops);
CREATE INDEX pending_usage_group ON olp.provider_resource_pending_usage(budget_group_id);
CREATE INDEX pending_usage_project ON olp.provider_resource_pending_usage(project_id);
CREATE INDEX pending_usage_key_project ON olp.provider_resource_pending_usage(api_key_project_id, end_user_digest);
CREATE INDEX pending_usage_provider ON olp.provider_resource_pending_usage(provider_id);
CREATE INDEX pending_usage_slot ON olp.provider_resource_pending_usage(slot_id);
CREATE INDEX pending_usage_route ON olp.provider_resource_pending_usage(route_id);
CREATE INDEX pending_usage_pinned_route ON olp.provider_resource_pending_usage(pinned_route_id);
CREATE INDEX pending_usage_leg_route ON olp.provider_resource_pending_usage(leg_route_id);
CREATE TABLE olp.pending_resource_recorded_owners (
    resource_id uuid NOT NULL,
    ordinal integer NOT NULL,
    budget_owner_id uuid NOT NULL,
    PRIMARY KEY(resource_id,ordinal,budget_owner_id),
    FOREIGN KEY(resource_id,ordinal) REFERENCES olp.provider_resource_pending_usage(resource_id,ordinal) ON DELETE CASCADE
);
CREATE INDEX pending_usage_recorded_owners ON olp.pending_resource_recorded_owners(budget_owner_id);
CREATE FUNCTION olp.project_pending_recorded_owners() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO olp.pending_resource_recorded_owners(resource_id,ordinal,budget_owner_id)
    SELECT NEW.resource_id,NEW.ordinal,owner FROM unnest(NEW.budget_owner_ids) owner
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END $$;
CREATE TRIGGER pending_recorded_owners AFTER INSERT ON olp.provider_resource_pending_usage
 FOR EACH ROW EXECUTE FUNCTION olp.project_pending_recorded_owners();

-- One derivation serves the trigger and upgrade backfill.
CREATE FUNCTION olp.pending_resource_subjects(pr olp.provider_resources)
RETURNS SETOF olp.provider_resource_pending_usage LANGUAGE sql STABLE AS $$
SELECT pr.id,(a->>'ordinal')::integer,pr.api_key_id,
           NULLIF(pr.metadata->'pending_usage'->>'budget_group_id','')::uuid,
           COALESCE(k.project_id,p.project_id,r.project_id),k.project_id,
           r.id,leg_r.id,pinned_r.id,ARRAY(SELECT b::uuid FROM jsonb_array_elements_text(COALESCE(a->'routing'->'budgets','[]'::jsonb)) b),pr.metadata->'pending_usage'->>'route_slug',(a->>'provider_id')::uuid,
           NULLIF(a->'routing'->>'credential_slot_id','')::uuid,
           COALESCE(pr.metadata->'pending_usage'->'attribution','{}'::jsonb),
           NULLIF(pr.metadata->'pending_usage'->>'end_user_digest','')
    FROM jsonb_array_elements(CASE WHEN olp.resource_owns_pending_usage(pr.kind)
         THEN pr.metadata->'pending_usage'->'attempts' ELSE '[]'::jsonb END) a
    JOIN olp.api_keys k ON k.id=pr.api_key_id
    JOIN olp.providers p ON p.id=(a->>'provider_id')::uuid
    LEFT JOIN olp.routes r ON r.slug=pr.metadata->'pending_usage'->>'route_slug'
    LEFT JOIN olp.routes pinned_r ON pinned_r.slug=pr.route_slug
    LEFT JOIN olp.routes leg_r ON leg_r.slug=a->'routing'->'leg'->>'route'
    WHERE NOT COALESCE((a->'routing'->>'budget_exempt')::boolean,false);
$$;

CREATE FUNCTION olp.project_pending_resource_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT olp.resource_owns_pending_usage(NEW.kind) THEN
        DELETE FROM olp.provider_resource_pending_usage WHERE resource_id=NEW.id;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.kind IS NOT DISTINCT FROM OLD.kind
       AND NEW.metadata->'pending_usage' IS NOT DISTINCT FROM OLD.metadata->'pending_usage' THEN
        RETURN NEW;
    END IF;
    DELETE FROM olp.provider_resource_pending_usage WHERE resource_id=NEW.id;
    IF NEW.metadata->'pending_usage' IS NULL OR NEW.metadata->'pending_usage' = 'null'::jsonb THEN
        RETURN NEW;
    END IF;
    INSERT INTO olp.provider_resource_pending_usage SELECT * FROM olp.pending_resource_subjects(NEW);
    RETURN NEW;
END
$$;
CREATE TRIGGER project_pending_resource_usage
    AFTER INSERT OR UPDATE OF metadata,kind ON olp.provider_resources
    FOR EACH ROW EXECUTE FUNCTION olp.project_pending_resource_usage();

-- Backfill already accepted Responses through the same derivation, preserving
-- their original accounting identities.
INSERT INTO olp.provider_resource_pending_usage
SELECT subjects.* FROM olp.provider_resources pr
CROSS JOIN LATERAL olp.pending_resource_subjects(pr) subjects;

-- A deferred generation must have a terminal accounting owner even when the
-- provider omitted expires_at. Existing native retained mappings keep the
-- same twenty-four-hour contract bound used by encrypted response ownership.
UPDATE olp.provider_resources SET expires_at=created_at+interval '24 hours'
WHERE olp.resource_owns_pending_usage(kind) AND metadata ? 'pending_usage' AND expires_at IS NULL;

CREATE INDEX provider_resources_expiry ON olp.provider_resources(expires_at,id)
 WHERE expires_at IS NOT NULL;

-- Resolve owner bindings under one snapshot without an unqualified heap probe:
-- projection replacement can leave a large dead prefix until the next vacuum.
-- Parameterized EXECUTE gives each indexed subject predicate its concrete scope;
-- a correlated generic plan otherwise scans an occupied project to disprove
-- unrelated ownership.
CREATE FUNCTION olp.has_pending_resource_usage(owner uuid) RETURNS boolean LANGUAGE plpgsql STABLE AS $$
DECLARE
    pending boolean;
    a olp.aggregate_budget_accounts;
    e olp.end_user_accounts;
    projects uuid[];
    column_name text;
BEGIN
    FOREACH column_name IN ARRAY ARRAY['api_key_id','budget_group_id','provider_id','slot_id','route_id','pinned_route_id','leg_route_id'] LOOP
        EXECUTE format('SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE %I=$1)',column_name)
            INTO pending USING owner;
        IF pending THEN RETURN true; END IF;
    END LOOP;
    EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.pending_resource_recorded_owners WHERE budget_owner_id=$1)'
        INTO pending USING owner;
    IF pending THEN RETURN true; END IF;
    SELECT * INTO a FROM olp.aggregate_budget_accounts WHERE id=owner;
    IF FOUND THEN
        IF a.installation_id IS NOT NULL THEN
            -- Installation owns every pending subject, including subjects that
            -- predate its cap. The ordered single-row probe can use the primary
            -- key without walking a dead heap prefix; an empty projection still
            -- has no pending owner.
            EXECUTE 'SELECT (SELECT resource_id FROM olp.provider_resource_pending_usage ORDER BY resource_id,ordinal LIMIT 1) IS NOT NULL'
                INTO pending;
        ELSIF a.organization_id IS NOT NULL THEN
            SELECT array_agg(id) INTO projects FROM olp.projects WHERE organization_id=a.organization_id;
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id=ANY($1))'
                INTO pending USING projects;
        ELSIF a.project_id IS NOT NULL AND a.attribution_key IS NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id=$1)'
                INTO pending USING a.project_id;
        ELSIF a.project_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id=$1 AND attribution @> $2)'
                INTO pending USING a.project_id,jsonb_build_object(a.attribution_key,a.attribution_value);
        ELSIF a.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_id=$1 AND route_slug=$2)'
                INTO pending USING a.api_key_id,a.route_slug;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    SELECT * INTO e FROM olp.end_user_accounts WHERE id=owner;
    IF FOUND THEN
        IF e.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_id=$1 AND end_user_digest=$2)'
                INTO pending USING e.api_key_id,e.end_user_digest;
        ELSE
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_project_id=$1 AND end_user_digest=$2)'
                INTO pending USING e.project_id,e.end_user_digest;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    RETURN false;
END
$$;
