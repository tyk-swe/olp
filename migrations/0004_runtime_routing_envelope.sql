-- Run with gateways drained. Old binaries cannot decode either the current
-- release or historical fallbacks after this migration. Snapshot identities,
-- credential encryption context, media references, and historical facts remain.
CREATE FUNCTION olp_v3.wrap_routing_release(payload bytea, checksum bytea)
RETURNS bytea LANGUAGE plpgsql AS $$
DECLARE document jsonb;
BEGIN
  IF sha256(payload) <> checksum THEN RETURN payload; END IF;
  BEGIN
    document := convert_from(payload, 'UTF8')::jsonb;
  EXCEPTION WHEN OTHERS THEN RETURN payload;
  END;
  IF jsonb_typeof(document) <> 'object' OR document ? 'format'
     OR NOT (document ?& ARRAY['generation','providers','routes','api_keys']) THEN
    RETURN payload;
  END IF;
  RETURN convert_to(jsonb_build_object('format','olp-routing-v1','snapshot',document)::text,'UTF8');
END $$;
WITH wrapped AS (
  SELECT id, compiled_release AS previous,
         olp_v3.wrap_routing_release(compiled_release, release_sha256) AS payload
  FROM olp_v3.runtime_generations
)
UPDATE olp_v3.runtime_generations r
SET compiled_release = w.payload, release_sha256 = sha256(w.payload)
FROM wrapped w WHERE r.id=w.id AND w.payload <> w.previous;
DROP FUNCTION olp_v3.wrap_routing_release(bytea, bytea);
