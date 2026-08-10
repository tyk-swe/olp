local version = 1
local values = redis.call('HMGET', KEYS[1], 'open_until_ms', 'probe_until_ms')
if not values[1] then
  return {version, 1}
end
local time = redis.call('TIME')
local now_ms = (tonumber(time[1]) * 1000) + math.floor(tonumber(time[2]) / 1000)
local open_until_ms = tonumber(values[1]) or 0
local probe_until_ms = tonumber(values[2]) or 0
if now_ms < open_until_ms or now_ms < probe_until_ms then
  return {version, 0}
end
return {version, 1}
