local values = redis.call('HMGET', KEYS[1], 'probe_token', 'probe_until_ms')
local probe_token = values[1]
local supplied_probe = ARGV[1] ~= ''
if supplied_probe or probe_token then
  if not probe_token or probe_token ~= ARGV[1] then
    return 0
  end
end
redis.call('DEL', KEYS[1])
return 1
