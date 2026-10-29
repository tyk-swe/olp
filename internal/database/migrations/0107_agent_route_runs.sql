ALTER TABLE olp.agent_runs ALTER COLUMN revision_id DROP NOT NULL;
ALTER TABLE olp.agent_runs ADD COLUMN route_revision_id uuid REFERENCES olp.route_revisions(id);
ALTER TABLE olp.agent_runs ADD CONSTRAINT agent_runs_revision_kind CHECK ((revision_id IS NULL) <> (route_revision_id IS NULL));
ALTER TABLE olp.execution_debit_results DROP CONSTRAINT execution_debit_results_event_id_key;
ALTER TABLE olp.execution_debit_results ADD CONSTRAINT execution_debit_results_dispatch_event_key UNIQUE (dispatch_id, event_id);
