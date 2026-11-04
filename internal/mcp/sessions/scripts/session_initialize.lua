-- KEYS: session hash.
-- Returns 1 when the session exists and was marked initialized, 0 when it
-- does not exist, so an expired session is never resurrected as a bare hash.

if redis.call("EXISTS", KEYS[1]) == 0 then
  return 0
end
redis.call("HSET", KEYS[1], "initialized", "1")
return 1
