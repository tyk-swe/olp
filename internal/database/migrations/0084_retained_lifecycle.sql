-- Resource payloads stay at the provider. Submission fences contain only
-- identities and a digest, and deliberately have no expiry or retry worker.
ALTER TABLE olp.provider_resources DROP CONSTRAINT provider_resources_kind_check;
ALTER TABLE olp.provider_resources ADD CONSTRAINT provider_resources_kind_check CHECK (kind IN (
 'file','batch','response','continuation','strict_response','interaction',
 'strict_file','strict_batch','anthropic_file','anthropic_batch','gemini_upload',
 'gemini_file','gemini_batch','cloud_batch','fine_tuning_job','fine_tuning_event',
 'fine_tuning_checkpoint','vector_store','vector_store_file','vector_store_batch',
 'conversation','conversation_item','container','container_file','eval','eval_run',
 'eval_output_item','skill','skill_version','context_cache','agent_resource'
));

DROP INDEX olp.provider_resources_native_identity;
CREATE UNIQUE INDEX provider_resources_native_identity ON olp.provider_resources
 (kind,api_key_id,provider_id,provider_revision_id,route_slug,route_revision_id,
  target_id,slot_id,credential_id,upstream_id) NULLS NOT DISTINCT
 WHERE kind IN ('file','batch','response','continuation','strict_response','interaction',
  'strict_file','strict_batch','anthropic_file','anthropic_batch','gemini_upload',
  'gemini_file','gemini_batch','cloud_batch');
CREATE UNIQUE INDEX retained_resources_native_identity ON olp.provider_resources
 (kind,api_key_id,provider_id,provider_revision_id,route_slug,route_revision_id,
  target_id,slot_id,credential_id,upstream_id,parent_id) NULLS NOT DISTINCT
 WHERE kind NOT IN ('file','batch','response','continuation','strict_response','interaction',
  'strict_file','strict_batch','anthropic_file','anthropic_batch','gemini_upload',
  'gemini_file','gemini_batch','cloud_batch');

CREATE TABLE olp.retained_submissions (
 resource_id uuid PRIMARY KEY REFERENCES olp.provider_resources(id),
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 operation text NOT NULL,
 submission_key text NOT NULL CHECK (octet_length(submission_key) BETWEEN 1 AND 128),
 request_digest bytea NOT NULL CHECK (octet_length(request_digest)=32),
 state text NOT NULL CHECK (state IN ('prepared','dispatched','accepted','submission_unknown','not_accepted')),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(api_key_id,operation,submission_key)
);
COMMENT ON TABLE olp.retained_submissions IS
 'At-most-once creation fences. Unknown acceptance never authorizes redispatch or expires with resource retention.';
