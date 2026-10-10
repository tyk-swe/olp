ALTER TABLE olp.provider_credentials ADD COLUMN external_reference jsonb;
ALTER TABLE olp.provider_credentials ADD CONSTRAINT provider_credential_reference_shape
 CHECK (external_reference IS NULL OR
   (jsonb_typeof(external_reference)='object' AND octet_length(external_reference::text)<=8192 AND plugin_digest IS NULL));

CREATE FUNCTION olp.immutable_credential_reference() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.external_reference IS DISTINCT FROM NEW.external_reference THEN
  RAISE EXCEPTION 'credential reference versions are immutable';
 END IF;
 RETURN NEW;
END
$$;
CREATE TRIGGER immutable_credential_reference BEFORE UPDATE OF external_reference
 ON olp.provider_credentials FOR EACH ROW EXECUTE FUNCTION olp.immutable_credential_reference();
