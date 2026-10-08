CREATE TABLE olp.workload_issuers (
 id uuid PRIMARY KEY,
 issuer text NOT NULL UNIQUE,
 document jsonb NOT NULL CHECK (jsonb_typeof(document)='object'),
 created_by uuid NOT NULL REFERENCES olp.users,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 etag uuid NOT NULL
);
ALTER TABLE olp.api_keys ADD COLUMN workload_issuer_id uuid REFERENCES olp.workload_issuers;
ALTER TABLE olp.api_keys ADD COLUMN workload_digest text CHECK(workload_digest ~ '^[0-9a-f]{64}$');
ALTER TABLE olp.api_keys ADD CONSTRAINT api_key_workload_identity CHECK ((workload_issuer_id IS NULL)=(workload_digest IS NULL));
CREATE UNIQUE INDEX api_key_workload_subject ON olp.api_keys(workload_digest) WHERE workload_digest IS NOT NULL;
-- These principals share the existing accounting, revocation and resource owner
-- model. Their digest column cannot authenticate an API secret; only a verified
-- workload JWT may select them.

ALTER TABLE olp.api_keys ADD COLUMN workload_mapping text;
ALTER TABLE olp.api_keys ADD CONSTRAINT api_key_workload_mapping CHECK((workload_issuer_id IS NULL)=(workload_mapping IS NULL));
DROP VIEW olp.api_keys_with_limits;
CREATE VIEW olp.api_keys_with_limits AS SELECT k.*,olp.effective_limits(k.policy,p.limit_templates->(k.policy->>'limit_template')) AS effective_limits FROM olp.api_keys k LEFT JOIN olp.projects p ON p.id=k.project_id;
