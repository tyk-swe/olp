local values = redis.call('HMGET', KEYS[1], 'open_until_ms', 'probe_until_ms', 'state_revision')
if not values[1] then
  return {'closed', '', ''}
end
local time = redis.call('TIME')
local now_ms = (tonumber(time[1]) * 1000) + math.floor(tonumber(time[2]) / 1000)
local open_until_ms = tonumber(values[1]) or 0
local probe_until_ms = tonumber(values[2]) or 0
local state_revision = values[3] or ''
if now_ms < open_until_ms or now_ms < probe_until_ms then
  return {'denied', '', state_revision}
end
-- A retained failure counter below the threshold is still a closed circuit.
if open_until_ms == 0 and probe_until_ms == 0 then
  return {'closed', '', state_revision}
end
local probe_until = now_ms + tonumber(ARGV[1])
redis.call('HSET', KEYS[1], 'probe_until_ms', probe_until, 'probe_token', ARGV[3])
redis.call('PEXPIRE', KEYS[1], math.max(tonumber(ARGV[1]), tonumber(ARGV[2])))
return {'probe', ARGV[3], state_revision}
