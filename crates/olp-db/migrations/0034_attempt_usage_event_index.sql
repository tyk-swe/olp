-- no-transaction
CREATE INDEX CONCURRENTLY attempt_usage_facts_event_id_idx
    ON attempt_usage_facts(event_id);
