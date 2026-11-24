DROP FUNCTION IF EXISTS olp.scale_price_lines(jsonb, numeric);
DROP FUNCTION IF EXISTS olp.price_usage_lines(jsonb, jsonb, timestamp with time zone, boolean, numeric);
DROP FUNCTION IF EXISTS olp.price_usage(jsonb, jsonb, timestamp with time zone, boolean, numeric);
DROP FUNCTION IF EXISTS olp.price_first_rate(jsonb, VARIADIC text[]);
