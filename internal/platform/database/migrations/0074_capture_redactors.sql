-- Capture publication pins immutable local detector revisions in execution order.
ALTER TABLE olp.capture_policies DROP CONSTRAINT capture_policies_redact_check;
ALTER TABLE olp.capture_policies ADD CONSTRAINT capture_policies_redact_check
 CHECK(cardinality(redact)<=128);
CREATE TABLE olp.capture_redactors (
 policy_id uuid NOT NULL REFERENCES olp.capture_policies(id) ON DELETE CASCADE,
 position integer NOT NULL CHECK(position BETWEEN 0 AND 127),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 revision_id uuid NOT NULL REFERENCES olp.guardrail_revisions(id),
 PRIMARY KEY(policy_id,position)
);
