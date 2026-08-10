local active_probe_token = redis.call('HGET', KEYS[1], 'probe_token')
if not active_probe_token or active_probe_token ~= ARGV[1] then
  return 0
end
redis.call('HDEL', KEYS[1], 'probe_until_ms', 'probe_token')
return 1
