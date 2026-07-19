-- Retention is run by the worker mode. Values are explicit settings so operators
-- can tune them without changing the immutable release format.
INSERT INTO settings (key, value, etag, updated_by)
SELECT key, value, gen_random_uuid(), id
FROM users
CROSS JOIN (VALUES
    ('retention.requests_days', '30'),
    ('retention.usage_days', '90'),
    ('retention.audit_days', '365')
) AS defaults(key, value)
WHERE role = 'owner'
ORDER BY created_at
LIMIT 3
ON CONFLICT (key) DO NOTHING;

-- The initial migration can run before setup, in which case defaults are filled
-- by setup transaction instead. This migration deliberately does not install an
-- extension: V2 works with managed PostgreSQL services where extension creation
-- is restricted.
