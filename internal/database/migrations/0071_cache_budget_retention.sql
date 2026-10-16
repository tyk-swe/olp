-- Cache-hit budget debits survive retention independently of provider spend.
ALTER TABLE olp.attempt_usage_hourly
    ADD COLUMN cache_budget_cost numeric(24,12) NOT NULL DEFAULT 0 CHECK (cache_budget_cost >= 0),
    ADD COLUMN cache_budget_charge numeric(24,12) NOT NULL DEFAULT 0 CHECK (cache_budget_charge >= 0);
