local version = 1
local values = redis.call('HMGET', KEYS[1], 'probe_token', 'probe_until_ms')
local probe_token = values[1]
local supplied_probe = ARGV[1] ~= ''
if supplied_probe or probe_token then
  if not probe_token or probe_token ~= ARGV[1] then
    return {version, 0}
  end
  local time = redis.call('TIME')
  local now_ms = (tonumber(time[1]) * 1000) + math.floor(tonumber(time[2]) / 1000)
  if now_ms >= (tonumber(values[2]) or 0) then
    return {version, 0}
  end
end
redis.call('DEL', KEYS[1])
return {version, 1}
