CREATE TABLE olp.code_account_cooldowns (
    account_id uuid NOT NULL REFERENCES olp.code_accounts,
    model text CHECK (model <> ''),
    class text NOT NULL CHECK (class IN ('quota','entitlement','rate','capacity','concurrency')),
    retry_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX code_account_cooldowns_pk ON olp.code_account_cooldowns(account_id, coalesce(model, ''));
CREATE INDEX code_account_cooldowns_account ON olp.code_account_cooldowns(account_id);
