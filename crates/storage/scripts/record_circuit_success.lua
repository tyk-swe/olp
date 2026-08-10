local values = redis.call('HMGET', KEYS[1], 'probe_token', 'state_revision')
local probe_token = values[1]
local state_revision = values[2] or ''
local supplied_probe = ARGV[1] ~= ''
if supplied_probe or probe_token then
  if not probe_token or probe_token ~= ARGV[1] then
    return 0
  end
elseif state_revision ~= ARGV[2] then
  return 0
end
redis.call('DEL', KEYS[1])
return 1
