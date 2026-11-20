CREATE UNIQUE INDEX provider_resources_native_identity_reuse ON olp.provider_resources
 (kind,api_key_id,provider_id,provider_revision_id,route_slug,route_revision_id,
  target_id,slot_id,credential_id,upstream_id,
  (CASE WHEN state='deleted' AND kind IN ('file','strict_file') THEN id END)) NULLS NOT DISTINCT
 WHERE kind IN ('file','batch','response','continuation','strict_response','interaction',
  'strict_file','strict_batch','anthropic_file','anthropic_batch','gemini_upload',
  'gemini_file','gemini_batch','cloud_batch');

CREATE UNIQUE INDEX retained_resources_native_identity_reuse ON olp.provider_resources
 (kind,api_key_id,provider_id,provider_revision_id,route_slug,route_revision_id,
  target_id,slot_id,credential_id,upstream_id,parent_id,
  (CASE WHEN state='deleted' AND kind='vector_store' THEN id END)) NULLS NOT DISTINCT
 WHERE kind NOT IN ('file','batch','response','continuation','strict_response','interaction',
  'strict_file','strict_batch','anthropic_file','anthropic_batch','gemini_upload',
  'gemini_file','gemini_batch','cloud_batch');

DROP INDEX olp.provider_resources_native_identity, olp.retained_resources_native_identity;
ALTER INDEX olp.provider_resources_native_identity_reuse RENAME TO provider_resources_native_identity;
ALTER INDEX olp.retained_resources_native_identity_reuse RENAME TO retained_resources_native_identity;
