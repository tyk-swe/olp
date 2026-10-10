ALTER TABLE olp.provider_slots ADD COLUMN etag uuid NOT NULL DEFAULT gen_random_uuid();

CREATE FUNCTION olp.advance_credential_slot_etag() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (to_jsonb(NEW)-'etag') IS DISTINCT FROM (to_jsonb(OLD)-'etag') THEN
  NEW.etag := gen_random_uuid();
 END IF;
 RETURN NEW;
END;
$$;

CREATE TRIGGER credential_slot_etag BEFORE UPDATE ON olp.provider_slots
 FOR EACH ROW EXECUTE FUNCTION olp.advance_credential_slot_etag();
