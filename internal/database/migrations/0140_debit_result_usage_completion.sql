ALTER TABLE olp.execution_debit_results ADD COLUMN usage_complete boolean NOT NULL DEFAULT false;
ALTER TABLE olp.execution_debit_results DISABLE TRIGGER execution_debit_results_immutable;
UPDATE olp.execution_debit_results r SET usage_complete=true
  FROM olp.execution_debits d
 WHERE d.id=r.dispatch_id
   AND (SELECT bool_and(f.usage_complete) FROM olp.attempt_usage_facts f WHERE f.event_id=r.event_id AND (d.kind<>'model' OR f.attempt_id=d.id));
ALTER TABLE olp.execution_debit_results ENABLE TRIGGER execution_debit_results_immutable;
