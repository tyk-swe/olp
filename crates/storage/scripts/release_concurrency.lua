-- KEYS: concurrency zset
-- ARGV: lease_id
return redis.call('ZREM', KEYS[1], ARGV[1])
