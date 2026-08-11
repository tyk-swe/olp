-- Route and target row IDs are revision-local storage identities. Keep a
-- separate identity for rendezvous hashing so draft simulation remains stable
-- across activation, restore, and concurrent same-slug activation outcomes.
ALTER TABLE route_drafts
    ADD COLUMN routing_id uuid;
ALTER TABLE route_draft_targets
    ADD COLUMN routing_id uuid;
ALTER TABLE route_revisions
    ADD COLUMN routing_id uuid;
ALTER TABLE route_revision_targets
    ADD COLUMN routing_id uuid;

-- Preserve the score of every live configuration written before this schema:
-- live routes hashed their route ID and live targets hashed their row ID.
-- Populate source revisions before using them to retain restored draft affinity.
UPDATE route_revisions
SET routing_id = route_id
WHERE routing_id IS NULL;
UPDATE route_revision_targets
SET routing_id = id
WHERE routing_id IS NULL;

-- A legacy restored draft previously simulated with its transient row IDs, so
-- it could not predict activation. Prefer the source revision's live identity
-- for unchanged selections to avoid remapping an unchanged route on its first
-- post-upgrade activation.
UPDATE route_drafts AS draft
SET routing_id = revision.routing_id
FROM route_revisions AS revision
WHERE draft.based_on_revision_id = revision.id
  AND draft.routing_id IS NULL;
-- Activated source drafts normally do not have a based-on revision. Preserve
-- the live route identity they produced so reactivating one does not remap
-- affinity traffic merely because the schema was upgraded.
UPDATE route_drafts AS draft
SET routing_id = revision.routing_id
FROM route_revisions AS revision
WHERE revision.source_draft_id = draft.id
  AND revision.slug = draft.slug
  AND draft.routing_id IS NULL;
UPDATE route_drafts
SET routing_id = id
WHERE routing_id IS NULL;

-- Unchanged restored targets retain their source revision identity. Edited
-- targets are new routing choices and retain their draft row identity.
UPDATE route_draft_targets AS draft_target
SET routing_id = revision_target.routing_id
FROM route_drafts AS draft,
     route_revision_targets AS revision_target
WHERE draft.id = draft_target.route_draft_id
  AND revision_target.route_revision_id = draft.based_on_revision_id
  AND revision_target.position = draft_target.position
  AND revision_target.provider_model_id = draft_target.provider_model_id
  AND revision_target.priority = draft_target.priority
  AND revision_target.weight = draft_target.weight
  AND revision_target.timeout_ms = draft_target.timeout_ms
  AND draft_target.routing_id IS NULL;
UPDATE route_draft_targets AS draft_target
SET routing_id = revision_target.routing_id
FROM route_drafts AS draft,
     route_revisions AS revision,
     route_revision_targets AS revision_target
WHERE draft.id = draft_target.route_draft_id
  AND revision.source_draft_id = draft.id
  AND revision.slug = draft.slug
  AND revision.revision = (
      SELECT max(source.revision)
      FROM route_revisions AS source
      WHERE source.source_draft_id = draft.id
        AND source.slug = draft.slug
  )
  AND revision_target.route_revision_id = revision.id
  AND revision_target.position = draft_target.position
  AND revision_target.provider_model_id = draft_target.provider_model_id
  AND revision_target.priority = draft_target.priority
  AND revision_target.weight = draft_target.weight
  AND revision_target.timeout_ms = draft_target.timeout_ms
  AND draft_target.routing_id IS NULL;
UPDATE route_draft_targets
SET routing_id = id
WHERE routing_id IS NULL;

ALTER TABLE route_drafts
    ALTER COLUMN routing_id SET NOT NULL;
ALTER TABLE route_draft_targets
    ALTER COLUMN routing_id SET NOT NULL;
ALTER TABLE route_revisions
    ALTER COLUMN routing_id SET NOT NULL;
ALTER TABLE route_revision_targets
    ALTER COLUMN routing_id SET NOT NULL;
