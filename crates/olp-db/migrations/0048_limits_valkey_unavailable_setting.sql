-- Hard-limited keys fail closed while Valkey is unavailable unless an owner
-- opts into fail-open. The value is a setting so operators can flip it at
-- runtime; gateways poll it.
INSERT INTO settings (key, value, etag, updated_by)
SELECT 'limits.valkey_unavailable', 'fail_closed', gen_random_uuid(), id
FROM users
WHERE role = 'owner'
ORDER BY created_at
LIMIT 1
ON CONFLICT (key) DO NOTHING;
