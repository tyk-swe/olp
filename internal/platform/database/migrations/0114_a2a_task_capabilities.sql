ALTER TABLE olp.a2a_task_admissions
 ADD COLUMN callback_attempts integer NOT NULL DEFAULT 0 CHECK(callback_attempts BETWEEN 0 AND 128);

CREATE TABLE olp.a2a_task_capabilities (
 dispatch_id uuid PRIMARY KEY REFERENCES olp.execution_debits,
 token_digest bytea NOT NULL UNIQUE CHECK(octet_length(token_digest)=32),
 publication_id uuid NOT NULL REFERENCES olp.agent_publications,
 revision_id uuid NOT NULL REFERENCES olp.agent_publication_revisions,
 task_id uuid REFERENCES olp.a2a_tasks,
 expires_at timestamptz NOT NULL,
 caller_workload_revision_id uuid,
 caller_expires_at timestamptz,
 end_user_digest text NOT NULL DEFAULT '',
 delegation_token_digest bytea
);
