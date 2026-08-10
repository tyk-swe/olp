local version = 1
local values = redis.call('HMGET', KEYS[1], 'failures', 'probe_token', 'probe_until_ms', 'open_until_ms')
local active_probe = values[2]
local supplied_probe = ARGV[1] ~= ''
if supplied_probe or active_probe then
  if not active_probe or active_probe ~= ARGV[1] then
    return {version, 0}
  end
end
local failures = (tonumber(values[1]) or 0) + 1
local threshold = tonumber(ARGV[2])
local ttl_ms = math.max(tonumber(ARGV[3]), tonumber(ARGV[4]))
if active_probe or failures >= threshold then
  local time = redis.call('TIME')
  local now_ms = (tonumber(time[1]) * 1000) + math.floor(tonumber(time[2]) / 1000)
  local open_until_ms = math.max(tonumber(values[4]) or 0, now_ms + tonumber(ARGV[3]))
  ttl_ms = math.max(ttl_ms, open_until_ms - now_ms)
  redis.call('HSET', KEYS[1], 'failures', threshold, 'open_until_ms', open_until_ms)
  redis.call('HDEL', KEYS[1], 'probe_until_ms', 'probe_token')
else
  redis.call('HSET', KEYS[1], 'failures', failures, 'open_until_ms', 0)
end
redis.call('PEXPIRE', KEYS[1], ttl_ms)
return {version, 1}
