-- Retrieval bindings refer only to operator-managed backend resources. Each
-- revision includes its explicit grants and backend field mappings.
CREATE TABLE olp.retrieval_bindings (
 id uuid PRIMARY KEY,
 project_id uuid NOT NULL REFERENCES olp.projects,
 name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 100),
 latest_revision_id uuid NOT NULL,
 etag uuid NOT NULL,
 retired_at timestamptz,
 created_by uuid NOT NULL REFERENCES olp.users,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX retrieval_binding_name ON olp.retrieval_bindings(project_id,lower(name)) WHERE retired_at IS NULL;
CREATE TABLE olp.retrieval_binding_revisions (
 id uuid PRIMARY KEY,
 binding_id uuid NOT NULL REFERENCES olp.retrieval_bindings,
 revision bigint NOT NULL CHECK (revision>0),
 definition jsonb NOT NULL CHECK (jsonb_typeof(definition)='object'),
 created_by uuid NOT NULL REFERENCES olp.users,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(binding_id,revision)
);
ALTER TABLE olp.retrieval_bindings ADD CONSTRAINT retrieval_binding_latest
 FOREIGN KEY(latest_revision_id) REFERENCES olp.retrieval_binding_revisions DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX retrieval_binding_project ON olp.retrieval_bindings(project_id,id);
ALTER TABLE olp.prices DROP CONSTRAINT prices_provider_kind_check;
ALTER TABLE olp.prices ADD CONSTRAINT prices_provider_kind_check CHECK (provider_kind IN (
 'openai','anthropic','gemini','vertex_ai','bedrock','sagemaker','watsonx','azure_openai','azure_ai_search','openai_compatible','plugin'));

CREATE TRIGGER retrieval_binding_revision_immutable BEFORE UPDATE OR DELETE ON olp.retrieval_binding_revisions FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
