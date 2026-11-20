ALTER TABLE olp.code_accounts
    ADD COLUMN max_concurrency integer CHECK (max_concurrency > 0);

CREATE INDEX code_attempts_active_generation
    ON olp.code_attempts (account_id, created_at)
    WHERE account_id IS NOT NULL AND finished_at IS NULL AND operation <> 'prewarm';
