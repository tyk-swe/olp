-- Runtime compilation relies on statement-fresh READ COMMITTED snapshots
-- after taking the cross-replica publication lock. Older binaries used
-- REPEATABLE READ and could therefore publish a pre-lock snapshot after a
-- newer writer committed. Reject those publications during rolling upgrades
-- instead of allowing a later generation to roll configuration backward.
CREATE FUNCTION enforce_runtime_publication_isolation() RETURNS trigger AS $$
BEGIN
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'runtime publication requires READ COMMITTED transaction isolation'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER runtime_generations_isolation_guard
    BEFORE INSERT ON runtime_generations
    FOR EACH ROW EXECUTE FUNCTION enforce_runtime_publication_isolation();
