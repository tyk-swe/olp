CREATE INDEX agent_runs_project_id ON olp.agent_runs(project_id, id DESC);
CREATE INDEX agent_approvals_run_id ON olp.agent_approvals(run_id, id);
CREATE INDEX trace_spans_run_id ON olp.trace_spans(run_id, started_nanos DESC, id DESC) WHERE run_id IS NOT NULL;
