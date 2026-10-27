-- KEYS: session hash, principal session index. Both keys must carry the same
--       Valkey Cluster hash tag.
-- ARGV: session_id, ttl_ms, cap, installation_id, project_id,
--       principal_digest, protocol, toolset_id, toolset_revision_id,
--       release_id.
-- Returns 1 when the session was created, 0 when the principal already holds
-- cap live sessions. Index scores are expiry instants, so pruning and
-- counting stay bounded by the cap.

local server_time = redis.call("TIME")
local now_ms = tonumber(server_time[1]) * 1000 + math.floor(tonumber(server_time[2]) / 1000)
local ttl_ms = tonumber(ARGV[2])

redis.call("ZREMRANGEBYSCORE", KEYS[2], "-inf", now_ms)
if redis.call("ZCARD", KEYS[2]) >= tonumber(ARGV[3]) then
  return 0
end

redis.call("HSET", KEYS[1],
  "installation_id", ARGV[4],
  "project_id", ARGV[5],
  "principal_digest", ARGV[6],
  "protocol", ARGV[7],
  "toolset_id", ARGV[8],
  "toolset_revision_id", ARGV[9],
  "release_id", ARGV[10],
  "created_at_ms", now_ms,
  "last_active_at_ms", now_ms,
  "initialized", "0")
redis.call("PEXPIRE", KEYS[1], ttl_ms)
redis.call("ZADD", KEYS[2], now_ms + ttl_ms, ARGV[1])
redis.call("PEXPIRE", KEYS[2], ttl_ms)
return 1
