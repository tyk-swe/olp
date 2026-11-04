-- KEYS: session hash.
-- ARGV: server_id, certification state.
-- Returns 1 when the session exists and its certification was updated, 0
-- when it does not exist, so an expired session is never resurrected as a
-- bare hash.

if redis.call("EXISTS", KEYS[1]) == 0 then
  return 0
end
redis.call("HSET", KEYS[1], "cert:" .. ARGV[1], ARGV[2])
return 1
