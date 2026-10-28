ALTER TABLE olp.mcp_server_revisions
 ADD COLUMN unit_price numeric CHECK(unit_price>=0),
 ADD COLUMN tool_prices jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(tool_prices)='object'),
 ADD COLUMN calls_per_minute integer NOT NULL DEFAULT 60 CHECK(calls_per_minute BETWEEN 1 AND 60000),
 ADD COLUMN max_concurrency integer NOT NULL DEFAULT 8 CHECK(max_concurrency BETWEEN 1 AND 1024),
 ADD COLUMN per_key_calls_per_minute integer NOT NULL DEFAULT 60 CHECK(per_key_calls_per_minute BETWEEN 1 AND 60000),
 ADD COLUMN per_key_max_concurrency integer NOT NULL DEFAULT 8 CHECK(per_key_max_concurrency BETWEEN 1 AND 1024);
ALTER TABLE olp.mcp_tool_calls
 ADD COLUMN unit_price numeric CHECK(unit_price>=0),
 ADD COLUMN pricing_state text NOT NULL DEFAULT 'unpriced' CHECK(pricing_state IN ('priced','free','unpriced')),
 ADD COLUMN currency text CHECK(currency ~ '^[A-Z]{3}$'),
 ADD CHECK((pricing_state='unpriced')=(unit_price IS NULL));
