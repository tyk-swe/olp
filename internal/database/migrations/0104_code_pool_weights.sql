ALTER TABLE olp.code_pool_accounts ADD COLUMN weight bigint NOT NULL DEFAULT 1 CHECK (weight BETWEEN 1 AND 2147483647);
