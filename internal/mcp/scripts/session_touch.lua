-- KEYS: session hash, principal session index. Both keys must carry the same
--       Valkey Cluster hash tag.
-- ARGV: session_id, ttl_ms.
-- Returns the session hash after refreshing its idle expiry, or an empty
-- array when the session does not exist or has expired.

if redis.call("EXISTS", KEYS[1]) == 0 then
  redis.call("ZREM", KEYS[2], ARGV[1])
  return {}
end

local server_time = redis.call("TIME")
local now_ms = tonumber(server_time[1]) * 1000 + math.floor(tonumber(server_time[2]) / 1000)
local ttl_ms = tonumber(ARGV[2])

redis.call("HSET", KEYS[1], "last_active_at_ms", now_ms)
redis.call("PEXPIRE", KEYS[1], ttl_ms)
redis.call("ZADD", KEYS[2], now_ms + ttl_ms, ARGV[1])
redis.call("PEXPIRE", KEYS[2], ttl_ms)
return redis.call("HGETALL", KEYS[1])
