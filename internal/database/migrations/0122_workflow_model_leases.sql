CREATE TABLE olp.workflow_model_leases (
    dispatch_id uuid PRIMARY KEY REFERENCES olp.execution_debits(id) ON DELETE CASCADE,
    claim_id uuid NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE INDEX workflow_model_leases_expiry_idx ON olp.workflow_model_leases(expires_at);

CREATE TABLE olp.workflow_model_nonacceptance_proofs (
    dispatch_id uuid PRIMARY KEY REFERENCES olp.execution_debits(id),
    claim_id uuid NOT NULL,
    source text NOT NULL CHECK(source='transport_not_sent'),
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TRIGGER workflow_model_nonacceptance_proof_immutable BEFORE UPDATE OR DELETE ON olp.workflow_model_nonacceptance_proofs
    FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
