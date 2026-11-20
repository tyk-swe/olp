ALTER TABLE olp.a2a_tasks
 ADD COLUMN caller_workload_revision_id uuid,
 ADD COLUMN caller_expires_at timestamptz,
 ADD COLUMN state text NOT NULL DEFAULT 'outcome_unknown' CHECK(state IN ('active','completed','failed','canceled','outcome_unknown')),
 ADD COLUMN deadline_at timestamptz NOT NULL DEFAULT (now() + interval '10 minutes'),
 ADD COLUMN continuations integer NOT NULL DEFAULT 0 CHECK(continuations BETWEEN 0 AND 128),
 ADD COLUMN cancel_claimed_until timestamptz;

CREATE INDEX a2a_tasks_active_owner ON olp.a2a_tasks(api_key_id,state);

CREATE TABLE olp.a2a_task_admissions (
 dispatch_id uuid PRIMARY KEY REFERENCES olp.execution_debits,
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 task_id uuid REFERENCES olp.a2a_tasks,
 created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX a2a_task_admissions_owner ON olp.a2a_task_admissions(api_key_id);
