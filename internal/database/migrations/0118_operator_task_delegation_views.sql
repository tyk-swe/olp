ALTER TABLE olp.a2a_tasks ADD COLUMN callback_attempts integer CHECK (callback_attempts BETWEEN 0 AND 128);
ALTER TABLE olp.a2a_tasks ALTER COLUMN callback_attempts SET DEFAULT 0;

ALTER TABLE olp.agent_delegations ADD COLUMN id uuid NOT NULL DEFAULT uuidv7();
CREATE UNIQUE INDEX agent_delegations_public_id ON olp.agent_delegations(id);
CREATE INDEX agent_delegations_project_id_id ON olp.agent_delegations(project_id,id DESC);
CREATE INDEX a2a_tasks_project_id_id ON olp.a2a_tasks(project_id,id DESC);
CREATE INDEX a2a_task_capabilities_task_id ON olp.a2a_task_capabilities(task_id) WHERE task_id IS NOT NULL;

CREATE TABLE olp.a2a_task_callback_accounting (
 dispatch_id uuid NOT NULL REFERENCES olp.execution_debits(id),
 kind text NOT NULL CHECK (kind IN ('inference','tools')),
 accounting_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (dispatch_id,kind,accounting_id),
 UNIQUE (kind,accounting_id)
);
