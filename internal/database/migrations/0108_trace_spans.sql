CREATE TABLE olp.trace_spans (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 project_id uuid NOT NULL REFERENCES olp.projects,
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 principal_digest bytea NOT NULL CHECK (octet_length(principal_digest) = 32),
 trace_id bytea NOT NULL CHECK (octet_length(trace_id) = 16),
 span_id bytea NOT NULL CHECK (octet_length(span_id) = 8),
 parent_span_id bytea CHECK (parent_span_id IS NULL OR octet_length(parent_span_id) = 8),
 run_id uuid REFERENCES olp.agent_runs,
 attempt_id uuid REFERENCES olp.execution_debits,
 started_nanos numeric(20,0) NOT NULL,
 ended_nanos numeric(20,0) NOT NULL,
 category text NOT NULL CHECK (category IN ('chat', 'embeddings', 'text_completion', 'execute_tool', 'invoke_agent', 'create_agent', 'other')),
 status smallint NOT NULL CHECK (status BETWEEN 0 AND 2),
 attributes jsonb NOT NULL CHECK (jsonb_typeof(attributes) = 'object'),
 received_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(project_id, api_key_id, principal_digest, trace_id, span_id)
);
CREATE INDEX trace_spans_project_received ON olp.trace_spans(project_id, received_at);
CREATE INDEX trace_spans_received ON olp.trace_spans(received_at);
