-- Optional provider-scoped prices override connector-kind defaults. PostgreSQL
-- NULLS NOT DISTINCT keeps exactly one default and one entry per provider scope
-- for each model/operation within an immutable revision.
ALTER TABLE prices
    ADD COLUMN provider_id uuid REFERENCES providers(id) ON DELETE CASCADE;

ALTER TABLE prices DROP CONSTRAINT prices_pkey;
ALTER TABLE prices
    ADD CONSTRAINT prices_revision_scope_key
    UNIQUE NULLS NOT DISTINCT
    (pricing_revision_id, provider_kind, provider_id, model, operation);

CREATE INDEX prices_provider_override_idx
    ON prices (provider_id, model, operation)
    WHERE provider_id IS NOT NULL;

CREATE FUNCTION enforce_price_provider_kind() RETURNS trigger AS $$
BEGIN
    IF NEW.provider_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM providers
        WHERE id = NEW.provider_id AND kind = NEW.provider_kind
    ) THEN
        RAISE EXCEPTION 'pricing override provider kind does not match provider'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER prices_provider_kind_guard
BEFORE INSERT OR UPDATE OF provider_id, provider_kind ON prices
FOR EACH ROW EXECUTE FUNCTION enforce_price_provider_kind();
