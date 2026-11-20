-- A provider's resource identifiers are opaque and need only identify a
-- resource within the exact serving source that accepted it. Keep that source
-- independent of mutable accounting metadata and preserve every local UUID.
ALTER TABLE olp.provider_resources ADD COLUMN target_id uuid;

-- Infer only an unambiguous target from the original immutable route/provider
-- documents. A missing or malformed historical source remains unproven; never
-- substitute today's model, slot, credential, or publication.
WITH candidates AS (
    SELECT resource.id,
           CASE WHEN jsonb_typeof(target->'id') = 'string'
                     AND target->>'id' ~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
                THEN (target->>'id')::uuid END AS target_id
    FROM olp.provider_resources resource
    JOIN olp.route_revisions route
      ON route.id = resource.route_revision_id AND route.slug = resource.route_slug
    JOIN olp.provider_revisions provider
      ON provider.id = resource.provider_revision_id AND provider.provider_id = resource.provider_id
    CROSS JOIN LATERAL jsonb_array_elements(
        CASE WHEN jsonb_typeof(route.targets) = 'array' THEN route.targets ELSE '[]'::jsonb END
    ) target
    WHERE jsonb_typeof(resource.metadata->'upstream_model') = 'string'
      AND jsonb_typeof(target->'provider_id') = 'string'
      AND target->>'provider_id' = resource.provider_id::text
      AND jsonb_typeof(target->'provider_model') = 'string'
      AND target->>'provider_model' = resource.metadata->>'upstream_model'
      AND (
          SELECT count(*) FROM jsonb_array_elements(route.targets) peer
          WHERE jsonb_typeof(peer->'id') = 'string'
            AND lower(peer->>'id') = lower(target->>'id')
      ) = 1
      AND EXISTS (
          SELECT 1 FROM jsonb_array_elements(
              CASE WHEN jsonb_typeof(provider.models) = 'array' THEN provider.models ELSE '[]'::jsonb END
          ) model
          WHERE jsonb_typeof(model->'upstream_model') = 'string'
            AND model->>'upstream_model' = target->>'provider_model'
            AND jsonb_typeof(model->'id') = 'string'
            AND jsonb_typeof(target->'provider_model_id') = 'string'
            AND model->>'id' = target->>'provider_model_id'
      )
      AND EXISTS (
          SELECT 1 FROM jsonb_array_elements(
              CASE WHEN jsonb_typeof(provider.slots) = 'array' THEN provider.slots ELSE '[]'::jsonb END
          ) slot
          WHERE jsonb_typeof(slot->'id') = 'string'
            AND slot->>'id' = resource.slot_id::text
            AND slot->>'credential_id' IS NOT DISTINCT FROM resource.credential_id::text
      )
), proven AS (
    SELECT id, min(target_id::text)::uuid AS target_id
    FROM candidates
    GROUP BY id
    HAVING count(*) = 1 AND count(target_id) = 1
)
UPDATE olp.provider_resources resource SET target_id = proven.target_id
FROM proven WHERE resource.id = proven.id;

ALTER TABLE olp.provider_resources
    DROP CONSTRAINT provider_resources_kind_provider_id_upstream_id_key;
CREATE UNIQUE INDEX provider_resources_native_identity ON olp.provider_resources
    (kind, api_key_id, provider_id, provider_revision_id, route_slug,
     route_revision_id, target_id, slot_id, credential_id, upstream_id)
    NULLS NOT DISTINCT;
