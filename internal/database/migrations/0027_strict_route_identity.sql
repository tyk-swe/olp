-- Old readers may retain their last supported snapshot. Strictness therefore
-- belongs to a published route identity; migration uses a new, unseen slug.
ALTER TABLE olp.routes ADD COLUMN strict_contract boolean NOT NULL DEFAULT false;
UPDATE olp.routes r SET strict_contract=coalesce(v.fidelity->>'mode','legacy')='strict'
FROM olp.route_revisions v WHERE v.id=r.latest_revision_id;

DO $$ BEGIN
    IF EXISTS (
        SELECT 1 FROM olp.routes r JOIN olp.route_revisions v ON v.route_id=r.id
        WHERE r.strict_contract AND coalesce(v.fidelity->>'mode','legacy')<>'strict'
    ) THEN
        RAISE EXCEPTION 'strict route has non-strict publication history; reviewed identity migration required';
    END IF;
END $$;

CREATE FUNCTION olp.preserve_route_contract_identity() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.slug IS DISTINCT FROM OLD.slug OR NEW.strict_contract IS DISTINCT FROM OLD.strict_contract THEN
        RAISE EXCEPTION 'published route contract identity is immutable; use a new slug'
            USING ERRCODE='23514', CONSTRAINT='route_contract_identity';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER preserve_route_contract_identity BEFORE UPDATE ON olp.routes
FOR EACH ROW EXECUTE FUNCTION olp.preserve_route_contract_identity();

CREATE FUNCTION olp.check_route_revision_contract() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE strict_identity boolean; published_slug text;
BEGIN
    IF TG_OP='UPDATE' AND (
        NEW.route_id IS DISTINCT FROM OLD.route_id OR
        NEW.slug IS DISTINCT FROM OLD.slug OR
        NEW.fidelity IS DISTINCT FROM OLD.fidelity
    ) THEN
        RAISE EXCEPTION 'published route revision contract is immutable'
            USING ERRCODE='23514', CONSTRAINT='route_revision_contract_identity';
    END IF;
    SELECT strict_contract,slug INTO strict_identity,published_slug
    FROM olp.routes WHERE id=NEW.route_id FOR UPDATE;
    IF NEW.slug IS DISTINCT FROM published_slug OR
       (coalesce(NEW.fidelity->>'mode','legacy')='strict') IS DISTINCT FROM strict_identity THEN
        RAISE EXCEPTION 'route revision requires a different published identity'
            USING ERRCODE='23514', CONSTRAINT='route_revision_contract_identity';
    END IF;
    IF NEW.fidelity IS NULL AND EXISTS (
        SELECT 1 FROM olp.route_revisions v
        WHERE v.route_id=NEW.route_id AND v.id IS DISTINCT FROM NEW.id AND v.fidelity IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'published explicit route contract cannot be omitted by a revision writer'
            USING ERRCODE='23514', CONSTRAINT='route_revision_contract_identity';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER check_route_revision_contract BEFORE INSERT OR UPDATE ON olp.route_revisions
FOR EACH ROW EXECUTE FUNCTION olp.check_route_revision_contract();

-- A pre-contract writer can omit fidelity while compiling a new snapshot even
-- when it has not changed the strict revision itself. Refuse that publication.
CREATE FUNCTION olp.check_runtime_route_contracts() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    IF EXISTS (
        SELECT 1 FROM olp.routes r
        JOIN olp.route_revisions v ON v.id=r.latest_revision_id
        WHERE r.state='active'
        AND coalesce((NEW.snapshot->'routes'->r.slug->'fidelity')::jsonb,'null'::jsonb)
            IS DISTINCT FROM coalesce(v.fidelity,'null'::jsonb)
    ) THEN
        RAISE EXCEPTION 'runtime writer cannot publish a changed route contract'
            USING ERRCODE='23514', CONSTRAINT='runtime_route_contract_identity';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER check_runtime_route_contracts BEFORE INSERT ON olp.runtime_releases
FOR EACH ROW EXECUTE FUNCTION olp.check_runtime_route_contracts();
