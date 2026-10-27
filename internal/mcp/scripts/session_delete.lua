-- KEYS: session hash, principal session index. Both keys must carry the same
--       Valkey Cluster hash tag.
-- ARGV: session_id.
-- Returns 1 when a live session was removed, 0 when none existed. The index
-- member is removed either way so expired sessions stop counting at once.

local removed = redis.call("DEL", KEYS[1])
redis.call("ZREM", KEYS[2], ARGV[1])
return removed
