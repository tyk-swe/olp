-- Extend a cooldown only when the requested window outlasts the current one,
-- so a short cooldown from one replica never shortens a longer one from
-- another. A key without an expiry is left alone.
-- KEYS: cooldown key. ARGV: wanted duration in milliseconds.
local current = redis.call('PTTL', KEYS[1])
local wanted = tonumber(ARGV[1])
if current == -2 or (current >= 0 and current < wanted) then
  redis.call('SET', KEYS[1], '1', 'PX', wanted)
end
return 1
