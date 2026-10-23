-- RM5.1: Vertex tuning jobs persist under their own retained kind.
ALTER TABLE olp.provider_resources DROP CONSTRAINT provider_resources_kind_check;
ALTER TABLE olp.provider_resources ADD CONSTRAINT provider_resources_kind_check CHECK (kind IN (
 'file','batch','response','continuation','strict_response','interaction',
 'strict_file','strict_batch','anthropic_file','anthropic_batch','gemini_upload',
 'gemini_file','gemini_batch','cloud_batch','fine_tuning_job','fine_tuning_event',
 'fine_tuning_checkpoint','vector_store','vector_store_file','vector_store_batch',
 'conversation','conversation_item','container','container_file','eval','eval_run',
 'eval_output_item','skill','skill_version','context_cache','agent_resource',
 'vertex_tuning_job'
));
