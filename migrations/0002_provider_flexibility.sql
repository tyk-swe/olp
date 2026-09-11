ALTER TABLE olp_v3.providers ADD COLUMN options jsonb NOT NULL DEFAULT '{}';
ALTER TABLE olp_v3.provider_revisions ADD COLUMN options jsonb NOT NULL DEFAULT '{}';
ALTER TABLE olp_v3.runtime_generation_provider_configs ADD COLUMN options jsonb NOT NULL DEFAULT '{}';

-- A slot is a logical credential; versions keep their original encryption AAD.
CREATE TABLE olp_v3.provider_credential_slots (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES olp_v3.providers(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    is_default boolean NOT NULL DEFAULT false,
    enabled boolean NOT NULL DEFAULT true,
    priority integer NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 65535),
    weight integer NOT NULL DEFAULT 1 CHECK (weight > 0),
    selected_version_id uuid REFERENCES olp_v3.provider_credential_versions(id),
    allowed_models text[] NOT NULL DEFAULT '{}',
    allowed_routes text[] NOT NULL DEFAULT '{}',
    allowed_api_keys uuid[] NOT NULL DEFAULT '{}',
    requests_per_minute integer CHECK (requests_per_minute > 0),
    tokens_per_minute bigint CHECK (tokens_per_minute > 0),
    max_concurrency integer CHECK (max_concurrency > 0),
    validated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(provider_id, name),
    UNIQUE(provider_id, id),
    FOREIGN KEY (provider_id, selected_version_id) REFERENCES olp_v3.provider_credential_versions(provider_id, id)
);
CREATE UNIQUE INDEX provider_default_slot ON olp_v3.provider_credential_slots(provider_id) WHERE is_default;
INSERT INTO olp_v3.provider_credential_slots (id, provider_id, name, is_default, selected_version_id, validated_at)
SELECT id, id, 'Default', true, active_credential_version_id, last_probe_at FROM olp_v3.providers;
ALTER TABLE olp_v3.provider_credential_versions ADD COLUMN slot_id uuid;
UPDATE olp_v3.provider_credential_versions SET slot_id = provider_id;
ALTER TABLE olp_v3.provider_credential_versions ADD CONSTRAINT credential_slot_owner
    FOREIGN KEY (provider_id, slot_id) REFERENCES olp_v3.provider_credential_slots(provider_id, id);
CREATE TABLE olp_v3.provider_revision_credentials (
    provider_revision_id uuid NOT NULL REFERENCES olp_v3.provider_revisions(id) ON DELETE CASCADE,
    slot_id uuid NOT NULL REFERENCES olp_v3.provider_credential_slots(id),
    credential_version_id uuid NOT NULL REFERENCES olp_v3.provider_credential_versions(id),
    configuration jsonb NOT NULL,
    PRIMARY KEY (provider_revision_id, slot_id)
);
INSERT INTO olp_v3.provider_revision_credentials
SELECT pr.id, pr.provider_id, pr.credential_version_id, jsonb_build_object('id',pr.provider_id,'name','Default','enabled',true,'priority',0,'weight',1,'credential_version_id',pr.credential_version_id)
FROM olp_v3.provider_revisions pr WHERE pr.credential_version_id IS NOT NULL;

CREATE FUNCTION olp_v3.sync_default_credential_slot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO olp_v3.provider_credential_slots(id,provider_id,name,is_default)
    VALUES(NEW.id,NEW.id,'Default',true) ON CONFLICT(id) DO NOTHING;
  UPDATE olp_v3.provider_credential_slots SET selected_version_id = NEW.active_credential_version_id
    WHERE provider_id = NEW.id AND is_default;
  UPDATE olp_v3.provider_credential_versions SET slot_id = NEW.id
    WHERE id = NEW.active_credential_version_id AND slot_id IS NULL;
  RETURN NEW;
END $$;
CREATE TRIGGER sync_default_credential_slot AFTER UPDATE OF active_credential_version_id ON olp_v3.providers
FOR EACH ROW EXECUTE FUNCTION olp_v3.sync_default_credential_slot();

-- Credential access evidence is scoped to the selected model contract.
CREATE FUNCTION olp_v3.invalidate_pool_model_validation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owner_id uuid;
BEGIN
  IF TG_TABLE_NAME = 'provider_models' THEN
    IF TG_OP = 'UPDATE' AND NEW.upstream_model IS NOT DISTINCT FROM OLD.upstream_model AND NEW.enabled IS NOT DISTINCT FROM OLD.enabled THEN RETURN NULL; END IF;
    owner_id := COALESCE(NEW.provider_id, OLD.provider_id);
  ELSE
    SELECT provider_id INTO owner_id FROM olp_v3.provider_models
      WHERE id = COALESCE(NEW.provider_model_id, OLD.provider_model_id);
  END IF;
  UPDATE olp_v3.provider_credential_slots SET validated_at = NULL
    WHERE provider_id = owner_id AND NOT is_default;
  RETURN NULL;
END $$;
CREATE TRIGGER invalidate_pool_models AFTER INSERT OR UPDATE OR DELETE ON olp_v3.provider_models
FOR EACH ROW EXECUTE FUNCTION olp_v3.invalidate_pool_model_validation();
CREATE TRIGGER invalidate_pool_capabilities AFTER INSERT OR UPDATE OR DELETE ON olp_v3.model_capabilities
FOR EACH ROW EXECUTE FUNCTION olp_v3.invalidate_pool_model_validation();
