-- Route fidelity is strict or transformed, and every draft and revision states
-- it. A published slug changes fidelity through an ordinary new revision.
DROP TRIGGER check_runtime_route_contracts ON olp.runtime_releases;
DROP FUNCTION olp.check_runtime_route_contracts();
DROP TRIGGER check_route_revision_contract ON olp.route_revisions;
DROP FUNCTION olp.check_route_revision_contract();
DROP TRIGGER preserve_route_contract_identity ON olp.routes;
DROP FUNCTION olp.preserve_route_contract_identity();
ALTER TABLE olp.routes DROP COLUMN strict_contract;

ALTER TABLE olp.route_drafts
    DROP CONSTRAINT route_drafts_fidelity_check,
    ALTER COLUMN fidelity SET NOT NULL,
    ADD CONSTRAINT route_drafts_fidelity_check CHECK (
        jsonb_typeof(fidelity) = 'object'
        AND fidelity->>'mode' IN ('strict', 'transformed')
        AND fidelity - 'mode' = '{}'::jsonb
    );
ALTER TABLE olp.route_revisions
    DROP CONSTRAINT route_revisions_fidelity_check,
    ALTER COLUMN fidelity SET NOT NULL,
    ADD CONSTRAINT route_revisions_fidelity_check CHECK (
        jsonb_typeof(fidelity) = 'object'
        AND fidelity->>'mode' IN ('strict', 'transformed')
        AND fidelity - 'mode' = '{}'::jsonb
    );

CREATE FUNCTION olp.preserve_route_slug() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.slug IS DISTINCT FROM OLD.slug THEN
        RAISE EXCEPTION 'route slug is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'route_slug_identity';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER preserve_route_slug BEFORE UPDATE ON olp.routes
FOR EACH ROW EXECUTE FUNCTION olp.preserve_route_slug();

-- A published revision keeps its route, slug and fidelity, and its slug is
-- the slug of its route.
CREATE FUNCTION olp.check_route_revision_identity() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP = 'UPDATE' AND (
        NEW.route_id IS DISTINCT FROM OLD.route_id OR
        NEW.slug IS DISTINCT FROM OLD.slug OR
        NEW.fidelity IS DISTINCT FROM OLD.fidelity
    ) THEN
        RAISE EXCEPTION 'published route revision identity is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'route_revision_identity';
    END IF;
    IF NEW.slug IS DISTINCT FROM (SELECT slug FROM olp.routes WHERE id = NEW.route_id) THEN
        RAISE EXCEPTION 'route revision slug must match its route'
            USING ERRCODE = '23514', CONSTRAINT = 'route_revision_identity';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER check_route_revision_identity BEFORE INSERT OR UPDATE ON olp.route_revisions
FOR EACH ROW EXECUTE FUNCTION olp.check_route_revision_identity();

COMMENT ON COLUMN olp.provider_resources.contract_version IS
    'Version of the resource-owned encrypted interaction contract. Kinds that store an encrypted payload under a contract version, such as strict responses, files and batches, stay separate from the metadata-only kinds of transformed routes.';
